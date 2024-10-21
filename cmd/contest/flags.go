package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/spikeekips/mitum/util"
)

type LogLevel struct {
	level zerolog.Level
}

func (f *LogLevel) UnmarshalText(b []byte) error {
	l, err := zerolog.ParseLevel(string(b))
	if err != nil {
		return errors.WithStack(err)
	}

	f.level = l

	return nil
}

func (f LogLevel) String() string {
	return f.level.String()
}

type HostFlag struct {
	host       string
	dockerhost *url.URL
	base       string
}

func (f *HostFlag) UnmarshalText(b []byte) error {
	e := util.StringError("parse host flag")

	h, err := url.Parse(string(b))
	if err != nil {
		return e.Wrap(err)
	}

	// NOTE parse fragment for additional information
	frags, err := url.ParseQuery(h.Fragment)
	if err != nil {
		return e.Wrap(err)
	}

	if i := frags.Get("base"); i != "" {
		f.base = i
	}

	h.Fragment = ""

	switch {
	case h.String() == "localhost", strings.HasPrefix(h.String(), "127.0."):
		f.host = "localhost"
	case h.Scheme == "unix":
		f.host = "localhost"
		f.dockerhost = h
	case len(h.Host) < 1:
		return e.Errorf("empty host")
	case h.Scheme != "tcp":
		return e.Errorf("scheme is not tcp, %q", h)
	default:
		if len(h.Port()) < 1 {
			h.Host = fmt.Sprintf("%s:2376", h.Host)
		}

		f.host = h.Hostname()
		f.dockerhost = h
	}

	return nil
}

func (f HostFlag) MarshalZerologObject(e *zerolog.Event) {
	e.Str("host", f.host).Str("base", f.base)

	if f.dockerhost != nil {
		e.Stringer("dockerhost", f.dockerhost)
	}
}

func (f HostFlag) String() string {
	return f.dockerhost.String()
}
