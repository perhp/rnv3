package publish

import "net/textproto"

func mimeHeader(h map[string]string) textproto.MIMEHeader {
	out := textproto.MIMEHeader{}
	for k, v := range h {
		out.Set(k, v)
	}
	return out
}
