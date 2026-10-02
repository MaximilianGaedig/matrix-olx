// mautrix-olx - A Matrix-OLX puppeting bridge.
// Copyright (C) 2026 Maximilian Gaedig
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"golang.org/x/image/draw"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/MaximilianGaedig/mautrix-olx/pkg/olxapi"
)

var (
	_ bridgev2.ReadReceiptHandlingNetworkAPI = (*OLXClient)(nil)
	_ bridgev2.TypingHandlingNetworkAPI      = (*OLXClient)(nil)
	_ bridgev2.TagHandlingNetworkAPI         = (*OLXClient)(nil)
	_ bridgev2.DeleteChatHandlingNetworkAPI  = (*OLXClient)(nil)
	_ bridgev2.UserBlockingNetworkAPI        = (*OLXClient)(nil)
)

// OLX takes two kinds of attachments, told apart by file extension: images
// (JPEG and PNG) and office documents.
var documentExtensions = map[string]string{
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}

var documentMimes = func() map[string]string {
	out := make(map[string]string, len(documentExtensions))
	for ext, mimeType := range documentExtensions {
		out[mimeType] = ext
	}
	return out
}()

const (
	// The website shrinks photos above these limits before uploading them.
	maxImageBytes     = 5_000_000
	maxImageDimension = 10_000
	shrunkImageWidth  = 2000
	jpegQuality       = 88
)

var ErrUnsupportedAttachment = bridgev2.WrapErrorInStatus(errors.New("OLX only accepts images and PDF, Word or Excel documents")).
	WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true).WithErrorReason(event.MessageStatusUnsupported)

var ErrReadOnlyChat = bridgev2.WrapErrorInStatus(errors.New("this OLX chat is closed, messages can't be sent to it")).
	WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true).WithErrorReason(event.MessageStatusUnsupported)

func replaceExt(name, ext string) string {
	if name == "" {
		name = "image"
	}
	return strings.TrimSuffix(name, path.Ext(name)) + ext
}

// prepareImage makes an image something OLX takes: a JPEG or PNG within the
// size the website would send. Anything else is re-encoded as JPEG.
func prepareImage(data []byte, name, mimeType string) ([]byte, string, string, error) {
	cfg, format, cfgErr := image.DecodeConfig(bytes.NewReader(data))
	small := len(data) <= maxImageBytes && (cfgErr != nil || (cfg.Width <= maxImageDimension && cfg.Height <= maxImageDimension))
	if small && cfgErr == nil {
		switch format {
		case "jpeg":
			return data, replaceExt(name, ".jpg"), "image/jpeg", nil
		case "png":
			return data, replaceExt(name, ".png"), "image/png", nil
		}
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", "", fmt.Errorf("can't read %s image: %w", mimeType, err)
	}
	bounds := img.Bounds()
	if !small && bounds.Dx() > shrunkImageWidth {
		height := max(bounds.Dy()*shrunkImageWidth/bounds.Dx(), 1)
		scaled := image.NewRGBA(image.Rect(0, 0, shrunkImageWidth, height))
		draw.CatmullRom.Scale(scaled, scaled.Bounds(), img, bounds, draw.Src, nil)
		img = scaled
	} else if o, ok := img.(interface{ Opaque() bool }); !ok || !o.Opaque() {
		// JPEG has no transparency: put the picture on white rather than black.
		flat := image.NewRGBA(bounds)
		draw.Draw(flat, bounds, image.White, image.Point{}, draw.Src)
		draw.Draw(flat, bounds, img, bounds.Min, draw.Over)
		img = flat
	}
	var buf bytes.Buffer
	if err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", "", err
	}
	return buf.Bytes(), replaceExt(name, ".jpg"), "image/jpeg", nil
}

// prepareAttachment downloads a Matrix file and makes it acceptable to OLX.
func (c *OLXClient) prepareAttachment(ctx context.Context, content *event.MessageEventContent) ([]byte, string, string, error) {
	data, err := c.Main.Bridge.Bot.DownloadMedia(ctx, content.URL, content.File)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to download media from Matrix: %w", err)
	}
	name := content.GetFileName()
	mimeType := content.GetInfo().MimeType
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	if strings.HasPrefix(mimeType, "image/") {
		return prepareImage(data, name, mimeType)
	}
	ext := strings.ToLower(path.Ext(name))
	if canonical, ok := documentExtensions[ext]; ok {
		return data, name, canonical, nil
	}
	if ext, ok := documentMimes[mimeType]; ok {
		return data, replaceExt(name, ext), mimeType, nil
	}
	return nil, "", "", ErrUnsupportedAttachment
}

func (c *OLXClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if msg.Portal.Metadata.(*PortalMetadata).ReadOnly {
		return nil, ErrReadOnlyChat
	}
	content := msg.Content
	out := &olxapi.OutgoingMessage{MessageID: uuid.NewString()}
	switch content.MsgType {
	case event.MsgText, event.MsgNotice:
		out.Text = content.Body
	case event.MsgEmote:
		out.Text = "* " + content.Body
	case event.MsgImage, event.MsgFile:
		data, name, mimeType, err := c.prepareAttachment(ctx, content)
		if err != nil {
			return nil, err
		}
		attachment, err := c.API.Upload(ctx, data, name, mimeType)
		if err != nil {
			return nil, c.checkErr(fmt.Errorf("failed to upload attachment to OLX: %w", err))
		}
		out.Attachments = []olxapi.OutgoingAttachment{*attachment}
		out.Text = content.GetCaption()
	case event.MsgVideo, event.MsgAudio:
		return nil, ErrUnsupportedAttachment
	default:
		return nil, fmt.Errorf("%w %s", bridgev2.ErrUnsupportedMessageType, content.MsgType)
	}
	if strings.TrimSpace(out.Text) == "" && len(out.Attachments) == 0 {
		return nil, errors.New("message is empty")
	}
	// OLX echoes own messages back on the socket; the ID is ours to pick, so
	// the echo can be recognized whichever arrives first.
	txnID := networkid.TransactionID(out.MessageID)
	msg.AddPendingToIgnore(txnID)
	sentAt := time.Now()
	if _, err := c.API.SendMessage(ctx, string(msg.Portal.ID), out); err != nil {
		msg.RemovePending(txnID)
		return nil, c.checkErr(err)
	}
	c.touchConversation(string(msg.Portal.ID), sentAt)
	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        networkid.MessageID(out.MessageID),
			SenderID:  c.GetUserID(),
			Timestamp: sentAt,
		},
		RemovePending: txnID,
	}, nil
}

// HandleMatrixReadReceipt marks the chat as read. OLX has no per-message read
// state for the reader: a chat is read up to its end or not at all.
func (c *OLXClient) HandleMatrixReadReceipt(ctx context.Context, receipt *bridgev2.MatrixReadReceipt) error {
	conversationID := string(receipt.Portal.ID)
	if err := c.API.MarkRead(ctx, conversationID); err != nil {
		return c.checkErr(err)
	}
	c.stateLock.Lock()
	if state, ok := c.convs[conversationID]; ok && receipt.ReadUpTo.After(state.SelfReadUpTo) {
		state.SelfReadUpTo = receipt.ReadUpTo
	}
	c.stateLock.Unlock()
	return nil
}

func (c *OLXClient) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	if msg.Portal.Metadata.(*PortalMetadata).ReadOnly {
		return nil
	}
	return c.checkErr(c.API.SendTyping(ctx, string(msg.Portal.ID), msg.IsTyping))
}

// HandleRoomTag moves a chat in or out of OLX's trash or saved list when the
// tag standing for it is added to or removed from the room.
func (c *OLXClient) HandleRoomTag(ctx context.Context, msg *bridgev2.MatrixRoomTag) error {
	conversationID := string(msg.Portal.ID)
	meta := msg.Portal.Metadata.(*PortalMetadata)
	changed := false
	if archived := bridgev2.TagChange(msg, c.Main.Config.ArchiveTag); archived != nil && *archived != meta.Archived {
		if err := c.API.SetTrashed(ctx, conversationID, *archived); err != nil {
			return c.checkErr(fmt.Errorf("failed to update OLX trash: %w", err))
		}
		meta.Archived = *archived
		changed = true
	}
	if saved := bridgev2.TagChange(msg, c.Main.Config.SavedTag); saved != nil && *saved != meta.Observed {
		if err := c.API.SetSaved(ctx, conversationID, *saved); err != nil {
			return c.checkErr(fmt.Errorf("failed to update OLX saved list: %w", err))
		}
		meta.Observed = *saved
		changed = true
	}
	if !changed {
		return nil
	}
	c.stateLock.Lock()
	if state, ok := c.convs[conversationID]; ok {
		state.Archived, state.Observed = meta.Archived, meta.Observed
	}
	c.stateLock.Unlock()
	if err := msg.Portal.Save(ctx); err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to save portal after tag change")
	}
	return nil
}

// HandleMatrixDeleteChat removes the chat on OLX: to the trash, where it can
// be restored from, unless the bridge is configured to delete for good.
func (c *OLXClient) HandleMatrixDeleteChat(ctx context.Context, msg *bridgev2.MatrixDeleteChat) error {
	conversationID := string(msg.Portal.ID)
	if c.Main.Config.DeleteChatPermanently {
		return c.checkErr(c.API.DeleteConversation(ctx, conversationID))
	}
	if msg.Portal.Metadata.(*PortalMetadata).Archived {
		return nil
	}
	return c.checkErr(c.API.SetTrashed(ctx, conversationID, true))
}

func (c *OLXClient) HandleMatrixBlock(ctx context.Context, ghost *bridgev2.Ghost, blocked bool) error {
	if !c.Main.ValidateUserID(ghost.ID) {
		return fmt.Errorf("can't block %s: not an OLX user", ghost.ID)
	}
	return c.checkErr(c.API.SetBlocked(ctx, string(ghost.ID), blocked))
}
