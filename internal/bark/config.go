package bark

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	ServerURL         string
	PublicURL         string
	BasicAuthUser     string
	BasicAuthPassword string
	Timeout           time.Duration
	DefaultGroup      string
	DefaultLevel      string
	DefaultSound      string
}

func (c Config) Configured() bool {
	return strings.TrimSpace(c.ServerURL) != ""
}

func ValidateConfig(c Config) error {
	if c.ServerURL == "" {
		return nil
	}
	u, err := url.Parse(c.ServerURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("BARK_SERVER_URL must be a valid absolute http or https URL")
	}
	if u.Fragment != "" || u.RawQuery != "" || u.User != nil {
		return errors.New("BARK_SERVER_URL must not contain userinfo, query, or fragment")
	}

	if c.PublicURL != "" {
		pu, err := url.Parse(c.PublicURL)
		if err != nil || pu.Host == "" || (pu.Scheme != "http" && pu.Scheme != "https") {
			return errors.New("BARK_PUBLIC_URL must be a valid absolute http or https URL")
		}
		if pu.Fragment != "" || pu.RawQuery != "" || pu.User != nil {
			return errors.New("BARK_PUBLIC_URL must not contain userinfo, query, or fragment")
		}
	}

	return nil
}
