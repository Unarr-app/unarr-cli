package nntp

import (
	"errors"
	"strings"
)

// NormalizeMessageID accepts a bare ID or one balanced pair of angle brackets.
// IDs are opaque, but must be a single printable NNTP argument. Never repair
// embedded whitespace or delimiters: doing so could request a different article.
func NormalizeMessageID(id string) (string, error) {
	if strings.HasPrefix(id, "<") && strings.HasSuffix(id, ">") {
		id = id[1 : len(id)-1]
	}
	if id == "" {
		return "", errors.New("nntp: empty message ID")
	}
	for _, b := range []byte(id) {
		if b <= ' ' || b >= 127 || b == '<' || b == '>' {
			return "", errors.New("nntp: invalid message ID")
		}
	}
	return id, nil
}

func (c *Client) validateCredentials() error {
	for _, value := range []string{c.cfg.Username, c.cfg.Password} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("nntp: invalid authentication argument")
		}
	}
	return nil
}
