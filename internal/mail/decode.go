package mail

import (
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"strings"
)

func decode(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	}
	return r
}
