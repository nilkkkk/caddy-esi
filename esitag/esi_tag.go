package esitag

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/SchumacherFM/caddyesi/bufpool"
	"github.com/SchumacherFM/caddyesi/helpers"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// TemplateIdentifier if some strings contain these characters then a
// template.Template will be created. For now a resource key or an URL is
// supported.
const TemplateIdentifier = "{{"

// Conditioner does not represent your favorite shampoo but it gives you the
// possibility to define an expression which gets executed for every request to
// include the ESI resource or not.
type Conditioner interface {
	OK(r *http.Request) bool
}

type condition struct {
	*template.Template
}

func (c condition) OK(r *http.Request) bool {
	// todo
	return false
}

// Tag identifies an ESI tag by its start and end position in the HTML byte
// stream for replacing. If the HTML changes there needs to be a refresh call to
// re-parse the HTML.
type Tag struct {
	// Data from the micro service gathered in a goroutine.
	Data  []byte
	Start int // start position in the stream
	End   int // end position in the stream
}

// Entity represents a single fully parsed ESI tag
type Entity struct {
	RawTag            []byte
	Tag               Tag
	Resources         // Any 3rd party servers
	TTL               time.Duration
	Timeout           time.Duration
	OnError           string
	ForwardHeaders    []string
	ForwardHeadersAll bool
	ReturnHeaders     []string
	ReturnHeadersAll  bool
	Conditioner       // todo
}

// todo split into two regexs for better performance and use the single quote regex only then when the first one returns nothing
var regexESITagDouble = regexp.MustCompile(`([a-z]+)="([^"\r\n]+)"|([a-z]+)='([^'\r\n]+)'`)

// ParseRaw parses the RawTag field and fills the remaining fields of the
// struct.
func (et *Entity) ParseRaw() error {
	if len(et.RawTag) == 0 {
		return nil
	}
	et.Resources.Logf = log.Printf

	// it's kinda ridiculous because the ESI tag parser uses even sync.Pool to
	// reduce allocs and speed up processing and here we're relying on regex.
	// Usually those function for ESI tag parsing will only be called once and
	// then cached. we can optimize it later.
	matches := regexESITagDouble.FindAllSubmatch(et.RawTag, -1)

	srcCounter := 0
	for _, subs := range matches {
		if len(subs) != 5 {
			var bufSubs bytes.Buffer
			for _, s := range subs {
				bufSubs.Write(s)
				bufSubs.WriteRune('\n')
			}

			return errors.Errorf("[caddyesi] ESITag.ParseRaw: Incorrect number of regex matches: %q => All matches: %#v\nTag: %q", bufSubs, matches, et.RawTag)
		}
		// 1+2 defines the double quotes: key="product_234234"
		subsAttr := subs[1]
		subsVal := subs[2]
		if len(subsAttr) == 0 {
			// fall back to enclosed in single quotes: key='product_234234_{{ .r.Header.Get "myHeaderKey" }}'
			subsAttr = subs[3]
			subsVal = subs[4]
		}
		attr := string(bytes.ToLower(subsAttr)) // must be lower because we use lower case here
		value := string(bytes.TrimSpace(subsVal))

		switch attr {
		case "src":
			if err := et.parseResource(attr, srcCounter, value); err != nil {
				return errors.Errorf("[caddyesi] Failed to parse src %q in tag %q with error:\n%+v", value, et.RawTag, err)
			}
			srcCounter++
		case "key":
			if err := et.parseResource(attr, srcCounter, value); err != nil {
				return errors.Errorf("[caddyesi] Failed to parse key %q in tag %q with error:\n%+v", value, et.RawTag, err)
			}
			// do not increment srcCounter because we might have already added the src
		case "condition":
			if err := et.parseCondition(value); err != nil {
				return errors.Errorf("[caddyesi] Failed to parse condition %q in tag %q with error:\n%+v", value, et.RawTag, err)
			}
		case "onerror":
			et.OnError = value
		case "timeout":
			var err error
			et.Timeout, err = time.ParseDuration(value)
			if err != nil {
				return errors.Errorf("[caddyesi] ESITag.ParseRaw. Cannot parse duration in timeout: %s => %q\nTag: %q", err, value, et.RawTag)
			}
		case "ttl":
			var err error
			et.TTL, err = time.ParseDuration(value)
			if err != nil {
				return errors.Errorf("[caddyesi] ESITag.ParseRaw. Cannot parse duration in ttl: %s => %q\nTag: %q", err, value, et.RawTag)
			}
		case "forwardheaders":
			if value == "all" {
				et.ForwardHeadersAll = true
			} else {
				et.ForwardHeaders = helpers.CommaListToSlice(value)
			}
		case "returnheaders":
			if value == "all" {
				et.ReturnHeadersAll = true
			} else {
				et.ReturnHeaders = helpers.CommaListToSlice(value)
			}
			// default: ignore all other tags
		}
	}
	if len(et.Resources.Items) == 0 || srcCounter == 0 {
		return errors.Errorf("[caddyesi] ESITag.ParseRaw. src cannot be empty in Tag which requires at least one resource: %q", et.RawTag)
	}
	return nil
}

func (et *Entity) parseCondition(s string) error {
	tpl, err := template.New("condition").Parse(s)
	if err != nil {
		errors.Errorf("[caddyesi] ESITag.ParseRaw. Failed to parse %q as template with error: %s\nTag: %q", s, err, et.RawTag)
	}
	et.Conditioner = condition{Template: tpl}
	return nil
}

func (et *Entity) parseResource(attr string, idx int, val string) error {
	// check if the idx has already been added to the Items slice.
	itemsIndexIdx := -1
	for i, r := range et.Resources.Items {
		if r.Index == idx {
			itemsIndexIdx = i
			break
		}
	}
	// case src already added and now processing the key attribute
	if attr == "key" && itemsIndexIdx >= 0 {
		// r represents a pointer
		r := et.Resources.Items[itemsIndexIdx] // it must panic if wrong
		if err := r.applyKey(val); err != nil {
			return errors.Errorf("[caddyesi] ESITag.ParseRaw. Failed to parse %q as template with error: %s\nTag: %q", val, err, et.RawTag)
		}
		return nil
	}

	// new resource pointer
	r := NewResource()
	if itemsIndexIdx >= 0 {
		r = et.Resources.Items[itemsIndexIdx] // it must panic if wrong
	}

	r.IsURL = attr == "src" && strings.Contains(val, "://")
	switch {
	case r.IsURL && strings.Contains(val, TemplateIdentifier):
		var err error
		r.URLTemplate, err = template.New("resource_tpl").Parse(val)
		if err != nil {
			return errors.Errorf("[caddyesi] ESITag.ParseRaw. Failed to parse %q as template with error: %s\nTag: %q", val, err, et.RawTag)
		}
	case attr == "src":
		r.URL = val
	case attr == "key":
		if err := r.applyKey(val); err != nil {
			return errors.Errorf("[caddyesi] ESITag.ParseRaw. Failed to parse %q as template with error: %s\nTag: %q", val, err, et.RawTag)
		}
	}
	r.Index = idx
	et.Resources.Items = append(et.Resources.Items, r)
	return nil
}

// Entities represents a list of ESI tags found in one HTML page.
type Entities []*Entity

// ParseRaw parses all ESI tags
func (et Entities) ParseRaw() error {
	for i := range et {
		if err := et[i].ParseRaw(); err != nil {
			return errors.Wrapf(err, "[caddyesi] Entities ParseRaw failed at index %d", i)
		}
	}
	return nil
}

// String for debugging only!
func (et Entities) String() string {
	buf := bufpool.Get()
	defer bufpool.Put(buf)

	for i, e := range et {
		raw := e.RawTag
		e.RawTag = nil
		_, _ = fmt.Fprintf(buf, "%d: %#v\n", i, e)
		_, _ = fmt.Fprintf(buf, "%d: RawTag: %q\n\n", i, raw)
	}
	return buf.String()
}

// QueryResources runs in parallel to query all available backend services /
// resources which are available in the current page. The returned Tag slice
// does not guarantee to be ordered.
func (et Entities) QueryResources(r *http.Request) ([]Tag, error) {

	tags := make([]Tag, 0, len(et))
	g, ctx := errgroup.WithContext(r.Context())
	cTag := make(chan Tag)
	for _, e := range et {
		e := e
		g.Go(func() error {
			data, err := e.Resources.DoRequest(e.Timeout, r)
			if err != nil {
				return errors.Wrapf(err, "[esitag] QueryResources.Resources.DoRequest failed for Tag %q", e.RawTag)
			}
			t := e.Tag
			t.Data = data

			select {
			case cTag <- t:
			case <-ctx.Done():
				return errors.Wrap(ctx.Err(), "[esitag] Context Done!")
			}
			return nil
		})
	}
	go func() {
		g.Wait()
		close(cTag)
	}()

	for t := range cTag {
		tags = append(tags, t)
	}

	// Check whether any of the goroutines failed. Since g is accumulating the
	// errors, we don't need to send them (or check for them) in the individual
	// results sent on the channel.
	if err := g.Wait(); err != nil {
		return nil, errors.Wrap(err, "[esitag]")
	}

	return tags, nil
}
