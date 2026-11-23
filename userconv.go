package main

import (
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"
)

const callbackB64 = "base64:"

// userMessage turns a message as the account got it into the Bot API shape that fromMessage, msgHTML and textGrid
// read. from is the sender; only its id, bot flag, username and first name are kept.
func userMessage(m *tg.Message, chat *tg.User, from *tg.User) map[string]any {
	out := map[string]any{
		"message_id": m.ID,
		"date":       m.Date,
		"chat":       map[string]any{"id": chat.ID, "type": "private", "username": chat.Username},
	}
	if from != nil {
		out["from"] = map[string]any{"id": from.ID, "is_bot": from.Bot, "username": from.Username, "first_name": from.FirstName}
	}
	if m.EditDate != 0 {
		out["edit_date"] = m.EditDate
	}
	textKey, entKey := "text", "entities"
	if m.Media != nil {
		if media := userMedia(m.Media); media != nil {
			if _, preview := media["link_preview_options"]; !preview {
				textKey, entKey = "caption", "caption_entities"
			}
			for k, v := range media {
				out[k] = v
			}
		}
	}
	if m.Message != "" || textKey == "text" {
		out[textKey] = m.Message
	}
	if ents := userEntities(m.Entities); len(ents) > 0 {
		out[entKey] = ents
	}
	if rm := userMarkup(m.ReplyMarkup); rm != nil {
		out["reply_markup"] = rm
	}
	meta := map[string]any{}
	if m.Out {
		meta["out"] = true
	}
	if m.ViaBotID != 0 {
		meta["via_bot"] = m.ViaBotID
	}
	if m.GroupedID != 0 {
		meta["grouped_id"] = m.GroupedID
	}
	if m.InvertMedia {
		meta["invert_media"] = true
	}
	if len(meta) > 0 {
		out["user_meta"] = meta
	}
	return out
}

var camelWord = regexp.MustCompile(`[A-Z][a-z0-9]*`)

func userEntities(list []tg.MessageEntityClass) []any {
	var out []any
	for _, e := range list {
		o := map[string]any{"offset": e.GetOffset(), "length": e.GetLength()}
		switch t := e.(type) {
		case *tg.MessageEntityBold:
			o["type"] = "bold"
		case *tg.MessageEntityItalic:
			o["type"] = "italic"
		case *tg.MessageEntityUnderline:
			o["type"] = "underline"
		case *tg.MessageEntityStrike:
			o["type"] = "strikethrough"
		case *tg.MessageEntitySpoiler:
			o["type"] = "spoiler"
		case *tg.MessageEntityCode:
			o["type"] = "code"
		case *tg.MessageEntityPre:
			o["type"] = "pre"
			if t.Language != "" {
				o["language"] = t.Language
			}
		case *tg.MessageEntityTextURL:
			o["type"], o["url"] = "text_link", t.URL
		case *tg.MessageEntityURL:
			o["type"] = "url"
		case *tg.MessageEntityMention:
			o["type"] = "mention"
		case *tg.MessageEntityMentionName:
			o["type"], o["user"] = "text_mention", map[string]any{"id": t.UserID}
		case *tg.MessageEntityHashtag:
			o["type"] = "hashtag"
		case *tg.MessageEntityCashtag:
			o["type"] = "cashtag"
		case *tg.MessageEntityBotCommand:
			o["type"] = "bot_command"
		case *tg.MessageEntityEmail:
			o["type"] = "email"
		case *tg.MessageEntityPhone:
			o["type"] = "phone_number"
		case *tg.MessageEntityBankCard:
			o["type"] = "bank_card"
		case *tg.MessageEntityBlockquote:
			o["type"] = "blockquote"
			if t.Collapsed {
				o["type"] = "expandable_blockquote"
			}
		case *tg.MessageEntityCustomEmoji:
			o["type"], o["custom_emoji_id"] = "custom_emoji", strconv.FormatInt(t.DocumentID, 10)
		default:
			name := strings.TrimPrefix(e.TypeName(), "messageEntity")
			words := camelWord.FindAllString(strings.ToUpper(name[:1])+name[1:], -1)
			o["type"] = strings.ToLower(strings.Join(words, "_"))
		}
		out = append(out, o)
	}
	return out
}

func userMarkup(rm tg.ReplyMarkupClass) map[string]any {
	switch t := rm.(type) {
	case *tg.ReplyInlineMarkup:
		var rows []any
		for _, r := range t.Rows {
			var row []any
			for _, b := range r.Buttons {
				row = append(row, inlineButton(b))
			}
			rows = append(rows, row)
		}
		return map[string]any{"inline_keyboard": rows}
	case *tg.ReplyKeyboardMarkup:
		var rows []any
		for _, r := range t.Rows {
			var row []any
			for _, b := range r.Buttons {
				row = append(row, map[string]any{"text": b.GetText()})
			}
			rows = append(rows, row)
		}
		return map[string]any{"keyboard": rows}
	}
	return nil
}

func inlineButton(b tg.KeyboardInlineButton) map[string]any {
	o := map[string]any{"text": b.Text}
	if st, ok := b.GetStyle(); ok {
		switch {
		case st.BgPrimary:
			o["style"] = "primary"
		case st.BgDanger:
			o["style"] = "danger"
		case st.BgSuccess:
			o["style"] = "success"
		}
		if st.Icon != 0 {
			o["icon_custom_emoji_id"] = strconv.FormatInt(st.Icon, 10)
		}
	}
	switch t := b.Type.(type) {
	case *tg.InlineButtonTypeCallback:
		if utf8.Valid(t.Data) {
			o["callback_data"] = string(t.Data)
		} else {
			o["callback_data"] = callbackB64 + base64.StdEncoding.EncodeToString(t.Data)
		}
		if t.RequiresPassword {
			o["requires_password"] = true
		}
	case *tg.InlineButtonTypeURL:
		o["url"] = t.URL
	case *tg.InlineButtonTypeWebView:
		o["web_app"] = map[string]any{"url": t.URL}
	case *tg.InlineButtonTypeSwitchInline:
		if t.SamePeer {
			o["switch_inline_query_current_chat"] = t.Query
		} else {
			o["switch_inline_query"] = t.Query
		}
	case *tg.InlineButtonTypeCopy:
		o["copy_text"] = map[string]any{"text": t.CopyText}
	case *tg.InlineButtonTypeURLAuth:
		o["login_url"] = map[string]any{"url": t.URL}
	case *tg.InlineButtonTypeBuy:
		o["pay"] = true
	case *tg.InlineButtonTypeGame:
		o["callback_game"] = map[string]any{}
	case *tg.InlineButtonTypeUserProfile:
		o["user_profile"] = t.UserID
	case *tg.InlineButtonTypeDisabled:
		o["disabled"] = true
	}
	return o
}

// callbackBytes is the callback data of a button as Telegram has it.
func callbackBytes(data string) []byte {
	if rest, ok := strings.CutPrefix(data, callbackB64); ok {
		if b, err := base64.StdEncoding.DecodeString(rest); err == nil {
			return b
		}
	}
	return []byte(data)
}

// userMedia gives the Bot API media fields. The file_unique_id keys start with "u:" because user-side file ids are
// not Bot API ids.
func userMedia(media tg.MessageMediaClass) map[string]any {
	switch t := media.(type) {
	case *tg.MessageMediaPhoto:
		p, ok := t.Photo.(*tg.Photo)
		if !ok {
			return nil
		}
		w, h := photoSize(p, 0)
		return map[string]any{"photo": []any{map[string]any{"file_unique_id": "u:photo:" + strconv.FormatInt(p.ID, 10), "width": w, "height": h}}}
	case *tg.MessageMediaDocument:
		d, ok := t.Document.(*tg.Document)
		if !ok {
			return nil
		}
		f := map[string]any{"file_unique_id": "u:doc:" + strconv.FormatInt(d.ID, 10), "mime_type": d.MimeType, "file_size": d.Size}
		kind := "document"
		for _, a := range d.Attributes {
			switch at := a.(type) {
			case *tg.DocumentAttributeFilename:
				f["file_name"] = at.FileName
			case *tg.DocumentAttributeSticker:
				kind = "sticker"
			case *tg.DocumentAttributeAnimated:
				if kind != "sticker" {
					kind = "animation"
				}
			case *tg.DocumentAttributeVideo:
				if kind == "document" {
					kind = "video"
				}
				f["width"], f["height"] = at.W, at.H
			case *tg.DocumentAttributeAudio:
				kind = "audio"
				if at.Voice {
					kind = "voice"
				}
			}
		}
		return map[string]any{kind: f}
	case *tg.MessageMediaWebPage:
		if wp, ok := t.Webpage.(*tg.WebPage); ok {
			return map[string]any{"link_preview_options": map[string]any{"url": wp.URL}}
		}
	}
	return nil
}

// photoSize picks the largest size of the photo that is at most maxSide pixels on each side (0: no limit).
func photoSize(p *tg.Photo, maxSide int) (w, h int) {
	_, w, h = photoThumb(p, maxSide)
	return w, h
}

func photoThumb(p *tg.Photo, maxSide int) (typ string, w, h int) {
	for _, s := range p.Sizes {
		var sw, sh int
		var st string
		switch t := s.(type) {
		case *tg.PhotoSize:
			st, sw, sh = t.Type, t.W, t.H
		case *tg.PhotoSizeProgressive:
			st, sw, sh = t.Type, t.W, t.H
		default:
			continue
		}
		if maxSide > 0 && (sw > maxSide || sh > maxSide) {
			continue
		}
		if sw*sh > w*h {
			typ, w, h = st, sw, sh
		}
	}
	return typ, w, h
}
