package connector

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"maunium.net/go/mautrix/event"

	"github.com/MaximilianGaedig/mautrix-olx/pkg/olxapi"
)

func testConfig(t *testing.T) *Config {
	cfg := &Config{
		Sites:               []string{"pl"},
		DisplaynameTemplate: "{{.Name}} (OLX)",
		RoomNameTemplate:    "{{.Name}} · {{.Title}}",
		ArchiveTag:          event.RoomTagLowPriority,
		SavedTag:            event.RoomTagFavourite,
	}
	if err := cfg.PostProcess(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestTemplates(t *testing.T) {
	cfg := testConfig(t)
	if got := cfg.FormatDisplayname(DisplaynameParams{Name: "Kuba"}); got != "Kuba (OLX)" {
		t.Errorf("displayname = %q", got)
	}
	if got := cfg.FormatDisplayname(DisplaynameParams{}); got != "OLX user (OLX)" {
		t.Errorf("nameless displayname = %q", got)
	}
	if got := cfg.FormatRoomName(RoomNameParams{Name: "Kuba", Title: "Iphone 15 pro max"}); got != "Kuba · Iphone 15 pro max" {
		t.Errorf("room name = %q", got)
	}
	if got := cfg.FormatRoomName(RoomNameParams{Name: "Kuba"}); got != "Kuba" {
		t.Errorf("a room without an ad title must not end in a separator: %q", got)
	}
}

func TestExampleConfigParses(t *testing.T) {
	var cfg Config
	if err := yamlUnmarshal(ExampleConfig, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Presence.Enabled || cfg.Presence.PollInterval.Seconds() != 60 || cfg.Presence.MaxUsers != 80 ||
		!cfg.Sync.Archived || cfg.Sync.Interval.Minutes() != 30 ||
		cfg.ArchiveTag != event.RoomTagLowPriority || cfg.SavedTag != event.RoomTagFavourite ||
		cfg.ClientVersion == "" || cfg.DeleteChatPermanently || len(cfg.Sites) != 1 || cfg.Sites[0] != "pl" {
		t.Errorf("example config parsed as %+v", cfg)
	}
}

func TestTagFor(t *testing.T) {
	c := &OLXClient{Main: &OLXConnector{Config: *testConfig(t)}}
	for _, tc := range []struct {
		archived, observed bool
		want               event.RoomTag
	}{
		{false, false, ""},
		{false, true, event.RoomTagFavourite},
		{true, false, event.RoomTagLowPriority},
		{true, true, event.RoomTagLowPriority},
	} {
		if got := *c.tagFor(tc.archived, tc.observed); got != tc.want {
			t.Errorf("tagFor(%v, %v) = %q, want %q", tc.archived, tc.observed, got, tc.want)
		}
	}
	c.Main.Config.ArchiveTag = ""
	if got := *c.tagFor(true, true); got != event.RoomTagFavourite {
		t.Errorf("with the archive tag off a saved chat keeps its tag, got %q", got)
	}
}

func TestAdIDs(t *testing.T) {
	// Real pairs of an ad's number and the short ID in its address.
	for short, id := range map[string]int64{"19CUAR": 1058393869, "1cxMNV": 1101501295, "1cpzgh": 1099542613} {
		if got := encodeBase62(id); got != short {
			t.Errorf("encodeBase62(%d) = %q, want %q", id, got, short)
		}
		got, err := ParseAdID("https://www.olx.pl/d/oferta/huawei-p30-lite-CID99-ID" + short + ".html?reason=x")
		if err != nil || got != id {
			t.Errorf("ParseAdID(address with %s) = %d, %v", short, got, err)
		}
	}
	if got, err := ParseAdID(" 1101945349 "); err != nil || got != 1101945349 {
		t.Errorf("numeric ad ID: %d %v", got, err)
	}
	for _, bad := range []string{"", "hello", "https://www.olx.pl/", "12"} {
		if _, err := ParseAdID(bad); err == nil {
			t.Errorf("ParseAdID(%q) must fail", bad)
		}
	}
	if got := AdURL(olxapi.MustSite("ua"), "1058393869"); got != "https://www.olx.ua/d/oferta/x-ID19CUAR.html" {
		t.Errorf("AdURL = %q", got)
	}
	if AdURL(olxapi.MustSite("pl"), "") != "" || AdURL(olxapi.MustSite("pl"), "abc") != "" {
		t.Error("AdURL of a non-number must be empty")
	}
}

func TestAdTopic(t *testing.T) {
	var conv olxapi.Conversation
	err := json.Unmarshal([]byte(`{"id":"c1","context":{"id":1058393869,"title":"Huawei P30","context_data":{
		"ad_id":"1058393869","price":{"minor_amount":190000,"currency_code":"PLN","is_negotiable":true},
		"publication_status":"INACTIVE","extension":{"location":{"city_name":"Poznań","district_name":"Wilda"}}}}}`), &conv)
	if err != nil {
		t.Fatal(err)
	}
	want := "Huawei P30 — 1 900 zł (negotiable)\nPoznań, Wilda · ad inactive · ID 1058393869\nhttps://www.olx.pl/d/oferta/x-ID19CUAR.html"
	if got := adTopic(olxapi.MustSite("pl"), &conv); got != want {
		t.Errorf("topic =\n%s\nwant\n%s", got, want)
	}
	if got := adTopic(olxapi.MustSite("pl"), &olxapi.Conversation{}); got != "" {
		t.Errorf("a chat without an ad has no topic, got %q", got)
	}
	bare := olxapi.Conversation{Ad: &olxapi.Ad{ID: "1058393869", Title: "Huawei P30"}}
	if got := adTopic(olxapi.MustSite("pl"), &bare); !strings.HasPrefix(got, "Huawei P30\nID 1058393869\n") {
		t.Errorf("topic without ad details = %q", got)
	}
}

func TestSystemText(t *testing.T) {
	for _, tc := range []struct{ typ, extras, text, want string }{
		{"system", `{"text":"Rozmowa została zakończona","initiated_at":"2026-10-02T09:00:00Z"}`, "", "Rozmowa została zakończona"},
		{"custom:single_closed_question", `{"question_id":"q","text":"Czy przedmiot dotarł?","detailed_text":"Odpowiedz w aplikacji"}`, "", "Czy przedmiot dotarł?\nOdpowiedz w aplikacji"},
		{"custom:price_negotiation_widget_buyer_proposed", `{"proposal":{"price":{"cents":150000,"currency":"PLN"}}}`, "", "Price proposal from the buyer: 1 500 zł"},
		{"custom:price_negotiation_widget_seller_accepted", `{}`, "", "The seller accepted the proposed price"},
		{"custom:unknown", ``, "fallback", "fallback"},
	} {
		msg := &olxapi.Message{Type: tc.typ, Text: tc.text}
		if tc.extras != "" {
			msg.Extras = json.RawMessage(tc.extras)
		}
		if got := systemText(msg); got != tc.want {
			t.Errorf("systemText(%s) = %q, want %q", tc.typ, got, tc.want)
		}
	}
}

func TestPrepareImage(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 40, 30))
	for i := range img.Pix {
		img.Pix[i] = 0
	}
	img.SetNRGBA(1, 1, color.NRGBA{R: 255, A: 255})

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	data, name, mimeType, err := prepareImage(pngBuf.Bytes(), "shot.PNG", "image/png")
	if err != nil || name != "shot.png" || mimeType != "image/png" || !bytes.Equal(data, pngBuf.Bytes()) {
		t.Errorf("a small PNG goes through untouched: %q %q %v", name, mimeType, err)
	}

	var jpgBuf bytes.Buffer
	if err = jpeg.Encode(&jpgBuf, img, nil); err != nil {
		t.Fatal(err)
	}
	data, name, mimeType, err = prepareImage(jpgBuf.Bytes(), "photo.jpeg", "image/jpeg")
	if err != nil || name != "photo.jpg" || mimeType != "image/jpeg" || !bytes.Equal(data, jpgBuf.Bytes()) {
		t.Errorf("a small JPEG goes through untouched: %q %q %v", name, mimeType, err)
	}

	// Anything else that decodes is turned into a JPEG, transparency on white.
	paletted := image.NewPaletted(image.Rect(0, 0, 40, 30), color.Palette{color.Transparent, color.NRGBA{R: 255, A: 255}})
	paletted.SetColorIndex(1, 1, 1)
	var gifBuf bytes.Buffer
	if err = encodeGIF(&gifBuf, paletted); err != nil {
		t.Fatal(err)
	}
	data, name, mimeType, err = prepareImage(gifBuf.Bytes(), "anim.gif", "image/gif")
	if err != nil || name != "anim.jpg" || mimeType != "image/jpeg" {
		t.Fatalf("GIF conversion: %q %q %v", name, mimeType, err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(data))
	if err != nil || format != "jpeg" {
		t.Fatalf("converted image is %q: %v", format, err)
	}
	if r, g, b, _ := decoded.At(30, 20).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Errorf("transparent pixels must become white, got %d %d %d", r>>8, g>>8, b>>8)
	}

	if _, _, _, err = prepareImage([]byte("not an image"), "x.heic", "image/heic"); err == nil {
		t.Error("an undecodable image must fail")
	}
}

func TestAttachmentKinds(t *testing.T) {
	for name, want := range map[string]bool{"a.JPG": true, "b.jpeg": true, "c.png": true, "d.webp": true, "e.gif": true, "f.pdf": false, "g": false} {
		if got := isImageName(name); got != want {
			t.Errorf("isImageName(%q) = %v", name, got)
		}
	}
	if len(documentMimes) != len(documentExtensions) {
		t.Error("every document extension needs its own MIME type")
	}
	if replaceExt("", ".jpg") != "image.jpg" || replaceExt("a.b.webp", ".jpg") != "a.b.jpg" {
		t.Error("replaceExt is off")
	}
}

func TestWWWClientIsHTTP1Only(t *testing.T) {
	www, err := newHTTPClient("socks5://127.0.0.1:1080", true)
	if err != nil {
		t.Fatal(err)
	}
	transport := www.Transport.(*http.Transport)
	if transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil || len(transport.TLSNextProto) != 0 ||
		len(transport.TLSClientConfig.NextProtos) != 1 || transport.TLSClientConfig.NextProtos[0] != "http/1.1" {
		t.Error("the www client must not negotiate HTTP/2")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://www.olx.pl/", nil)
	if proxyURL, err := transport.Proxy(req); err != nil || proxyURL == nil || proxyURL.Host != "127.0.0.1:1080" {
		t.Errorf("proxy = %v, %v", proxyURL, err)
	}
	chat, err := newHTTPClient("", false)
	if err != nil {
		t.Fatal(err)
	}
	if !chat.Transport.(*http.Transport).ForceAttemptHTTP2 {
		t.Error("the chat client keeps HTTP/2")
	}
	if _, err = newHTTPClient("://bad", false); err == nil {
		t.Error("an invalid proxy address must be refused")
	}
}

func TestLoginFlows(t *testing.T) {
	oc := &OLXConnector{Config: *testConfig(t)}
	flows := oc.GetLoginFlows()
	if len(flows) != 1 || flows[0].ID != "pl" {
		t.Fatalf("one configured site is one flow, so that `login` needs no choice: %+v", flows)
	}
	oc.Config.Sites = []string{"ua", "olx.ro", "ua"}
	flows = oc.GetLoginFlows()
	if len(flows) != 2 || flows[0].ID != "ua" || flows[1].ID != "ro" {
		t.Fatalf("flows follow the configured sites, in order, once each: %+v", flows)
	}
	for _, flow := range flows {
		if len(flow.Description) > 60 {
			t.Errorf("flow descriptions are listed one per line and must stay short: %q", flow.Description)
		}
		if _, err := oc.CreateLogin(t.Context(), nil, flow.ID); err != nil {
			t.Errorf("offered flow %q is not accepted: %v", flow.ID, err)
		}
	}
	if _, err := oc.CreateLogin(t.Context(), nil, "xx"); err == nil {
		t.Error("an unknown site must be refused")
	}
	oc.Config.Sites = nil
	if flows = oc.GetLoginFlows(); len(flows) != 1 || flows[0].ID != olxapi.DefaultSite {
		t.Errorf("no configured sites means the default one: %+v", flows)
	}

	login := &OLXLogin{Main: oc, Site: olxapi.MustSite("ro")}
	step, err := login.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		// Whole, so that it is one copy and one paste.
		"```\nview-source:https://login.olx.ro/oauth2/authorize?",
		// And plain, for browsers without view-source.
		"(https://login.olx.ro/oauth2/authorize?",
		"view-source:https://www.olx.ro/d/callback/?code=", "history", "OLX.ro", tokenSnippet,
	} {
		if !strings.Contains(step.Instructions, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
	if strings.Contains(step.Instructions, login.pkce.Verifier) {
		t.Error("the verifier must not be shown")
	}
	if lines := strings.Count(step.Instructions, "\n"); lines > 20 {
		t.Errorf("instructions are %d lines long, keep them short", lines)
	}

	// One input field takes either what the login page ends on or a token.
	for input, wantToken := range map[string]bool{
		"view-source:https://www.olx.ro/d/callback/?code=0c7e2f3a-1111-2222-3333-444455556666&state=x": false,
		"https://www.olx.ro/d/callback/?code=abc":                                                      false,
		"0c7e2f3a-1111-2222-3333-444455556666":                                                         false,
		"eyJjdHkiOiJKV1QiLCJlbmMiOiJBMjU2R0NNIiwiYWxnIjoiUlNBLU9BRVAifQ." + strings.Repeat("Ab1_", 80): true,
		"`eyJjdHkiOiJKV1QifQ." + strings.Repeat("x", 300) + "`":                                        true,
	} {
		if got := looksLikeToken(cleanInput(input)); got != wantToken {
			t.Errorf("looksLikeToken(%.40q) = %v", input, got)
		}
	}
	for in, want := range map[string]string{" abc \n": "abc", `"abc"`: "abc", "`abc`": "abc", "'abc'": "abc"} {
		if got := cleanInput(in); got != want {
			t.Errorf("cleanInput(%q) = %q", in, got)
		}
	}
}

// The config upgrader runs on every start, on whatever config the admin has:
// it must take the example itself and a config from before a key existed.
func TestConfigUpgrade(t *testing.T) {
	oc := &OLXConnector{}
	_, _, upgrader := oc.GetConfig()
	for name, cfg := range map[string]string{
		"example":       ExampleConfig,
		"without sites": "displayname_template: \"{{.Name}}\"\ndefault_site: pl\nclient_version: abc\n",
		"several sites": "sites:\n    - ua\n    - ro\n",
	} {
		out, err := runConfigUpgrade(upgrader, cfg)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(out, "sites:") || !strings.Contains(out, "presence:") || strings.Contains(out, "default_site") {
			t.Errorf("%s: upgraded config is not the current shape:\n%s", name, out)
		}
		if name == "several sites" && !(strings.Contains(out, "- ua") && strings.Contains(out, "- ro")) {
			t.Errorf("configured sites must survive the upgrade:\n%s", out)
		}
	}
}
