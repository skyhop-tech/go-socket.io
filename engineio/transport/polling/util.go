package polling

import (
	"mime"
	"strings"

	"github.com/pkg/errors"
)

type Addr struct {
	Host string
}

func (a Addr) Network() string {
	return "tcp"
}

func (a Addr) String() string {
	return a.Host
}

func mimeIsSupportBinary(m string) (bool, error) {
	typ, params, err := mime.ParseMediaType(m)
	if err != nil {
		return false, errors.Wrap(err, "mimeIsSupportBinary: mime.ParseMediaType")
	}

	switch typ {
	case "application/octet-stream":
		return true, nil

	case "text/plain":
		charset := strings.ToLower(params["charset"])
		if charset != "utf-8" {
			return false, errors.New("mimeIsSupportBinary: invalid charset")
		}
		return false, nil
	}

	return false, errors.New("mimeIsSupportBinary: invalid content-type")
}
