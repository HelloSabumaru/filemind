package filemind

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DataDir, OwnerListen, PublicListen, OwnerURL, PublicURL                        string
	OwnerUsername, OwnerPasswordFile                                               string
	DefaultExpiry                                                                  time.Duration
	DefaultDownloadLimit, MaxFileSize, MaxTransferSize, StorageQuota, MinFreeSpace int64
	Development                                                                    bool
	TrustedProxies                                                                 []netip.Prefix
}

func ConfigFromEnv() (Config, error) {
	c := Config{DataDir: env("FILEMIND_DATA_DIR", "/data"), OwnerListen: env("FILEMIND_OWNER_LISTEN", ":8080"), PublicListen: env("FILEMIND_PUBLIC_LISTEN", ":8081"), OwnerURL: os.Getenv("FILEMIND_OWNER_URL"), PublicURL: os.Getenv("FILEMIND_PUBLIC_URL"), OwnerUsername: env("FILEMIND_OWNER_USERNAME", "admin"), OwnerPasswordFile: os.Getenv("FILEMIND_OWNER_PASSWORD_FILE")}
	var err error
	if c.Development, err = strconv.ParseBool(env("FILEMIND_INSECURE_DEVELOPMENT", "false")); err != nil {
		return c, errors.New("invalid FILEMIND_INSECURE_DEVELOPMENT")
	}
	if c.DefaultExpiry, err = time.ParseDuration(env("FILEMIND_DEFAULT_EXPIRY", "24h")); err != nil || c.DefaultExpiry < 0 {
		return c, errors.New("invalid FILEMIND_DEFAULT_EXPIRY")
	}
	for _, item := range []struct {
		name      string
		target    *int64
		fallback  int64
		allowZero bool
	}{
		{"FILEMIND_DEFAULT_DOWNLOAD_LIMIT", &c.DefaultDownloadLimit, 0, true},
		{"FILEMIND_MAX_FILE_SIZE", &c.MaxFileSize, 2147483648, false},
		{"FILEMIND_MAX_TRANSFER_SIZE", &c.MaxTransferSize, 2147483648, false},
		{"FILEMIND_STORAGE_QUOTA", &c.StorageQuota, 21474836480, false},
		{"FILEMIND_MIN_FREE_SPACE", &c.MinFreeSpace, 1073741824, true},
	} {
		v, e := strconv.ParseInt(env(item.name, strconv.FormatInt(item.fallback, 10)), 10, 64)
		if e != nil || v < 0 || (!item.allowZero && v == 0) {
			return c, fmt.Errorf("invalid %s", item.name)
		}
		*item.target = v
	}
	for _, value := range strings.Split(os.Getenv("FILEMIND_TRUSTED_PROXY_CIDRS"), ",") {
		if strings.TrimSpace(value) == "" {
			continue
		}
		p, e := netip.ParsePrefix(strings.TrimSpace(value))
		if e != nil || p.Bits() == 0 {
			return c, errors.New("invalid FILEMIND_TRUSTED_PROXY_CIDRS")
		}
		c.TrustedProxies = append(c.TrustedProxies, p.Masked())
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) == "/" {
		return errors.New("FILEMIND_DATA_DIR must be a dedicated absolute directory")
	}
	if c.OwnerUsername == "" || len(c.OwnerUsername) > 100 || c.OwnerPasswordFile == "" {
		return errors.New("owner username and password file are required")
	}
	if c.MaxFileSize <= 0 || c.MaxTransferSize < c.MaxFileSize || c.StorageQuota < c.MaxTransferSize || c.StorageQuota > 1<<50 || c.DefaultDownloadLimit < 0 || c.DefaultDownloadLimit > 1000000 || c.DefaultExpiry < 0 || c.DefaultExpiry > 365*24*time.Hour || c.DefaultExpiry%time.Second != 0 || c.MinFreeSpace < 0 {
		return errors.New("invalid storage or retention settings")
	}
	for _, origin := range []string{c.OwnerURL, c.PublicURL} {
		u, err := url.Parse(origin)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("origins must be absolute URLs without paths, credentials, queries or fragments")
		}
		if u.Port() != "" {
			port, e := strconv.Atoi(u.Port())
			if e != nil || port < 1 || port > 65535 {
				return errors.New("invalid origin port")
			}
		}
		if u.Scheme != "https" {
			if !c.Development || u.Scheme != "http" || !isLoopback(u.Hostname()) {
				return errors.New("HTTPS origins are required; development HTTP is restricted to localhost")
			}
		}
	}
	if canonicalOrigin(c.OwnerURL) == canonicalOrigin(c.PublicURL) {
		return errors.New("owner and public origins must differ")
	}
	for _, address := range []string{c.OwnerListen, c.PublicListen} {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return errors.New("invalid listener address")
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || (host != "" && net.ParseIP(host) == nil && host != "localhost") {
			return errors.New("invalid listener address")
		}
	}
	if c.OwnerListen == c.PublicListen {
		return errors.New("listeners must differ")
	}
	for _, prefix := range c.TrustedProxies {
		if !prefix.IsValid() || prefix.Bits() == 0 {
			return errors.New("invalid trusted proxy range")
		}
	}
	return nil
}

func canonicalOrigin(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return origin
	}
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	u.Path, u.RawPath = "", ""
	return u.String()
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
