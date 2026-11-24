package connector

import (
	"context"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

// Rift's GIF picker sends a GIF as markdown in the message text: ![GIF|220x183](https://static.klipy.com/…/x.webp).
// The web client renders such images only from Klipy hosts, so the bridge does the same.
var gifMarkdown = regexp.MustCompile(`!\[([^\]|]*)(?:\|(\d+)x(\d+))?\]\((https://[^)\s]+)\)`)

type gifRef struct {
	URL           string
	Title         string
	Width, Height int
}

func isKlipyURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "klipy.com" || strings.HasSuffix(h, ".klipy.com")
}

// splitGIFs removes Klipy GIF markdown from text and returns the remaining text and the GIFs.
func splitGIFs(text string) (string, []gifRef) {
	var gifs []gifRef
	rest := gifMarkdown.ReplaceAllStringFunc(text, func(m string) string {
		sm := gifMarkdown.FindStringSubmatch(m)
		if !isKlipyURL(sm[4]) {
			return m
		}
		w, _ := strconv.Atoi(sm[2])
		h, _ := strconv.Atoi(sm[3])
		gifs = append(gifs, gifRef{URL: sm[4], Title: sm[1], Width: w, Height: h})
		return ""
	})
	if len(gifs) == 0 {
		return text, nil
	}
	return strings.TrimSpace(rest), gifs
}

func (rc *RiftClient) convertGIF(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, gif gifRef) *bridgev2.ConvertedMessagePart {
	part := &bridgev2.ConvertedMessagePart{ID: networkid.PartID(gif.URL), Type: event.EventMessage}
	name := path.Base(gif.URL)
	data, mimeType, err := rc.client.DownloadURL(ctx, gif.URL)
	if err == nil {
		if !strings.HasPrefix(mimeType, "image/") && !strings.HasPrefix(mimeType, "video/") {
			mimeType = "image/webp"
		}
		content := &event.MessageEventContent{MsgType: event.MsgImage, Body: name}
		if strings.HasPrefix(mimeType, "video/") {
			content.MsgType = event.MsgVideo
		}
		content.URL, content.File, err = intent.UploadMedia(ctx, portal.MXID, data, name, mimeType)
		if err == nil {
			content.Info = &event.FileInfo{MimeType: mimeType, Size: len(data), Width: gif.Width, Height: gif.Height}
			content.Info.IsAnimated = true
			content.Info.MauGIF = true
			part.Content = content
			return part
		}
	}
	zerolog.Ctx(ctx).Err(err).Str("url", gif.URL).Msg("Failed to bridge GIF")
	part.Content = &event.MessageEventContent{MsgType: event.MsgText, Body: gif.URL}
	return part
}
