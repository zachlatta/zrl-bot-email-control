package gmailfake

import (
	"bytes"
	"io"
	"mime/quotedprintable"
)

func newQPReader(body []byte) io.Reader {
	return quotedprintable.NewReader(bytes.NewReader(body))
}
