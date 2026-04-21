// Copyright (C) 2025 wangyusong
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/glidea/zenfeed/pkg/util/buffer"
)

const (
	AppName = "zenfeed"
	Module  = "github.com/glidea/zenfeed"
)

// LabelXXX is the metadata label for the feed.
const (
	LabelType    = "type"
	LabelSource  = "source"
	LabelTitle   = "title"
	LabelLink    = "link"
	LabelPubTime = "pub_time"
	LabelContent = "content"
)

// Feed is core data model for a feed.
//
//	E.g. {
//	 "labels": {
//	   "title": "The most awesome feed management software of 2025 has been born",
//	   "content": "....",
//	   "link": "....",
//	 },
//	 "time": "2025-01-01T00:00:00Z",
//	}
type Feed struct {
	ID     uint64    `json:"id,string"`
	Labels Labels    `json:"labels"`
	Time   time.Time `json:"time"`
}

func (f *Feed) Validate() error {
	if len(f.Labels) == 0 {
		return errors.New("labels is required")
	}
	for i := range f.Labels {
		l := &f.Labels[i]
		if l.Key == "" {
			return errors.New("label key is required")
		}
	}
	if f.Time.IsZero() {
		f.Time = time.Now()
	}

	return nil
}

type Labels []Label

func (ls *Labels) FromMap(m map[string]string) {
	*ls = make(Labels, 0, len(m))
	for k, v := range m {
		*ls = append(*ls, Label{Key: k, Value: v})
	}
	ls.EnsureSorted()
}

func (ls Labels) Map() map[string]string {
	m := make(map[string]string, len(ls))
	for _, l := range ls {
		m[l.Key] = l.Value
	}

	return m
}

func (ls Labels) String() string {
	ls.EnsureSorted()
	var b strings.Builder
	for i, l := range ls {
		b.WriteString(l.Key)
		b.WriteString(": ")
		b.WriteString(l.Value)
		if i < len(ls)-1 {
			b.WriteString(",")
		}
	}

	return b.String()
}

func (ls Labels) Get(key string) string {
	for _, l := range ls {
		if l.Key != key {
			continue
		}

		return l.Value
	}

	return ""
}

func (ls *Labels) Put(key, value string, sort bool) {
	for i, l := range *ls {
		if l.Key != key {
			continue
		}
		(*ls)[i].Value = value

		return
	}
	*ls = append(*ls, Label{Key: key, Value: value})
	if sort {
		ls.EnsureSorted()
	}
}

func (ls Labels) MarshalJSON() ([]byte, error) {
	ls.EnsureSorted()

	buf := buffer.Get()
	defer buffer.Put(buf)

	if _, err := buf.WriteString("{"); err != nil {
		return nil, errors.Wrap(err, "write starting brace for Labels object")
	}

	for i, l := range ls {
		if _, err := fmt.Fprintf(buf, "\"%s\":", l.Key); err != nil {
			return nil, errors.Wrap(err, "write label key")
		}

		escapedVal, err := json.Marshal(l.Value)
		if err != nil {
			return nil, errors.Wrap(err, "marshal label value")
		}
		if _, err := buf.Write(escapedVal); err != nil {
			return nil, errors.Wrap(err, "write label value")
		}

		if last := i == len(ls)-1; !last {
			if _, err := buf.WriteString(","); err != nil {
				return nil, errors.Wrap(err, "write comma for Labels object")
			}
		}
	}

	if _, err := buf.WriteString("}"); err != nil {
		return nil, errors.Wrap(err, "write ending brace for Labels object")
	}

	return buf.Bytes(), nil
}

func (ls *Labels) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))

	// Expect starting '{'
	if err := readExpectedDelim(dec, '{'); err != nil {
		return errors.Wrap(err, "read starting brace for Labels object")
	}

	// Read key-value pairs.
	var labels Labels
	for dec.More() {
		key, value, err := readKeyValue(dec)
		if err != nil {
			return errors.Wrapf(err, "read key-value pair for Labels object")
		}

		labels = append(labels, Label{Key: key, Value: value})
	}

	// Expect starting '}'
	if err := readExpectedDelim(dec, '}'); err != nil {
		return errors.Wrap(err, "read ending brace for Labels object")
	}

	// Ensure sorted.
	*ls = labels
	ls.EnsureSorted()

	return nil
}

func (ls Labels) EnsureSorted() {
	if !ls.sorted() {
		ls.sort()
	}
}

func (ls Labels) sorted() bool {
	sorted := true
	for i := range len(ls) - 1 {
		if ls[i].Key > ls[i+1].Key {
			sorted = false

			break
		}
	}

	return sorted
}

func (ls Labels) sort() {
	sort.Slice(ls, func(i, j int) bool {
		return ls[i].Key < ls[j].Key
	})
}

type Label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

const (
	LabelFilterEqual    = "="
	LabelFilterNotEqual = "!="
)

type LabelFilter struct {
	Label string
	Equal bool
	Value string
}

func NewLabelFilter(filter string) (LabelFilter, error) {
	eq := false
	parts := strings.Split(filter, LabelFilterNotEqual)
	if len(parts) != 2 {
		parts = strings.Split(filter, LabelFilterEqual)
		eq = true
	}
	if len(parts) != 2 {
		return LabelFilter{}, errors.New("invalid label filter")
	}

	return LabelFilter{Label: parts[0], Value: parts[1], Equal: eq}, nil
}

func (f LabelFilter) Match(labels Labels) bool {
	lv := labels.Get(f.Label)
	if lv == "" {
		return false
	}

	if f.Equal && lv == f.Value {
		return true
	}
	if !f.Equal && lv != f.Value {
		return true
	}

	return false
}

type LabelFilters []LabelFilter

func (ls LabelFilters) Match(labels Labels) bool {
	if len(ls) == 0 {
		return true // No filters, always match.
	}

	for _, l := range ls {
		if !l.Match(labels) {
			return false
		}
	}

	return true
}

func NewLabelFilters(filters []string) (LabelFilters, error) {
	ls := make(LabelFilters, len(filters))
	for i, f := range filters {
		lf, err := NewLabelFilter(f)
		if err != nil {
			return nil, errors.Wrapf(err, "new label filter %q", f)
		}
		ls[i] = lf
	}

	return ls, nil
}

// readExpectedDelim reads the next token and checks if it's the expected delimiter.
func readExpectedDelim(dec *json.Decoder, expected json.Delim) error {
	t, err := dec.Token()
	if err != nil {
		return errors.Wrapf(err, "read token")
	}

	delim, ok := t.(json.Delim)
	if !ok || delim != expected {
		return errors.Errorf("expected '%c' delimiter, got %T %v", expected, t, t)
	}

	return nil
}

// readKeyValue reads a single key-value pair from the JSON object.
// Assumes the key is a string and the value decodes into a string.
func readKeyValue(dec *json.Decoder) (key string, value string, err error) {
	// Read key.
	keyToken, err := dec.Token()
	if err != nil {
		return "", "", errors.Wrap(err, "read key token")
	}
	keyStr, ok := keyToken.(string)
	if !ok {
		return "", "", errors.Errorf("expected string key, got %T %v", keyToken, keyToken)
	}

	// Read value.
	var valStr string
	if err := dec.Decode(&valStr); err != nil {
		return "", "", errors.Wrapf(err, "decode value for key %q", keyStr)
	}

	return keyStr, valStr, nil
}
