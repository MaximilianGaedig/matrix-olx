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
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
	_ "golang.org/x/image/webp"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/MaximilianGaedig/mautrix-olx/pkg/olxapi"
)

const maxAttachmentSize = 100 << 20

func attachmentPartID(index int) networkid.PartID {
	return networkid.PartID("att" + strconv.Itoa(index))
}

// isImageName is OLX's own rule for what counts as an image: the extension.
func isImageName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return true
	default:
		return false
	}
}

func cleanMime(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return mediaType
}

// systemText is the text of a message that is not plain chat: OLX's own
// notices and its interactive widgets, which have no Matrix counterpart and
// are shown as what they say.
func systemText(msg *olxapi.Message) string {
	var extras struct {
		Text         string `json:"text"`
		DetailedText string `json:"detailed_text"`
		Proposal     struct {
			Price struct {
				Cents    *int64 `json:"cents"`
				Currency string `json:"currency"`
			} `json:"price"`
		} `json:"proposal"`
	}
	if len(msg.Extras) > 0 {
		_ = json.Unmarshal(msg.Extras, &extras)
	}
	price := ""
	if cents := extras.Proposal.Price.Cents; cents != nil {
		price = (&olxapi.Price{MinorAmount: cents, CurrencyCode: extras.Proposal.Price.Currency}).String()
	}
	withPrice := func(text string) string {
		if price != "" {
			return text + ": " + price
		}
		return text
	}
	switch msg.Type {
	case "custom:price_negotiation_widget_buyer_proposed":
		return withPrice("Price proposal from the buyer")
	case "custom:price_negotiation_widget_seller_proposed":
		return withPrice("Counter-offer from the seller")
	case "custom:price_negotiation_widget_seller_accepted":
		return withPrice("The seller accepted the proposed price")
	}
	text := strings.TrimSpace(extras.Text)
	if detail := strings.TrimSpace(extras.DetailedText); detail != "" && detail != text {
		text = strings.TrimSpace(text + "\n" + detail)
	}
	if text == "" {
		text = strings.TrimSpace(msg.Text)
	}
	return text
}

func (c *OLXClient) convertAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, att *olxapi.Attachment, caption string) *event.MessageEventContent {
	name := att.Name
	if name == "" {
		name = path.Base(att.URL)
	}
	failed := func(err error) *event.MessageEventContent {
		zerolog.Ctx(ctx).Err(err).Str("attachment_name", name).Msg("Failed to bridge attachment")
		body := fmt.Sprintf("Failed to bridge attachment %s", name)
		if caption != "" {
			body = caption + "\n\n" + body
		}
		return &event.MessageEventContent{MsgType: event.MsgNotice, Body: body}
	}
	if att.URL == "" {
		return failed(fmt.Errorf("attachment has no address"))
	}
	data, contentType, err := c.API.Download(ctx, att.URL, maxAttachmentSize)
	if err != nil {
		return failed(fmt.Errorf("download: %w", err))
	}
	mimeType := cleanMime(contentType)
	if mimeType == "" || mimeType == "application/octet-stream" || mimeType == "binary/octet-stream" {
		mimeType = http.DetectContentType(data)
		if byExt := mime.TypeByExtension(strings.ToLower(path.Ext(name))); byExt != "" && (mimeType == "application/octet-stream" || mimeType == "application/zip") {
			mimeType = cleanMime(byExt)
		}
	}
	content := &event.MessageEventContent{
		MsgType: event.MsgFile,
		Body:    name,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     len(data),
		},
	}
	if strings.HasPrefix(mimeType, "image/") || isImageName(name) {
		content.MsgType = event.MsgImage
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			content.Info.Width, content.Info.Height = cfg.Width, cfg.Height
		}
	}
	if caption != "" {
		content.FileName = name
		content.Body = caption
	}
	content.URL, content.File, err = intent.UploadMedia(ctx, portal.MXID, data, name, mimeType)
	if err != nil {
		return failed(fmt.Errorf("upload: %w", err))
	}
	return content
}

// convertMessage turns an OLX message into Matrix events: the text, then one
// event per attachment. A message that is one attachment with text becomes a
// single captioned file.
func (c *OLXClient) convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *olxapi.Message) (*bridgev2.ConvertedMessage, error) {
	if extras, ok := olxapi.ParseNegotiationExtras(msg); ok {
		return c.convertNegotiation(ctx, msg, extras), nil
	}
	converted := &bridgev2.ConvertedMessage{}
	text := msg.Text
	msgType := event.MsgText
	if msg.Type != "" && msg.Type != olxapi.MessageTypeStandard {
		msgType = event.MsgNotice
		text = systemText(msg)
		if text == "" && len(msg.Attachments) == 0 {
			text = "Unsupported OLX message (" + msg.Type + "), open the chat on OLX to see it."
		}
	}
	caption := ""
	if len(msg.Attachments) == 1 && msgType == event.MsgText {
		caption = strings.TrimSpace(text)
	} else if strings.TrimSpace(text) != "" {
		converted.Parts = append(converted.Parts, &bridgev2.ConvertedMessagePart{
			ID:      "",
			Type:    event.EventMessage,
			Content: &event.MessageEventContent{MsgType: msgType, Body: text},
		})
	}
	for i := range msg.Attachments {
		partID := attachmentPartID(i)
		if len(msg.Attachments) == 1 && len(converted.Parts) == 0 {
			partID = ""
		}
		converted.Parts = append(converted.Parts, &bridgev2.ConvertedMessagePart{
			ID:      partID,
			Type:    event.EventMessage,
			Content: c.convertAttachment(ctx, portal, intent, &msg.Attachments[i], caption),
		})
	}
	if len(converted.Parts) == 0 {
		converted.Parts = append(converted.Parts, &bridgev2.ConvertedMessagePart{
			Type:    event.EventMessage,
			Content: &event.MessageEventContent{MsgType: event.MsgNotice, Body: "Empty OLX message"},
		})
	}
	return converted, nil
}
