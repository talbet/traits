package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Object is a JSON object that keeps its keys in their original order, so
// rewriting a user's settings file changes only what was merged.
type Object struct {
	Keys   []string
	Values map[string]any // values are Object, []any, string, json.Number, bool or nil
}

// Get returns the value for key.
func (o *Object) Get(key string) (any, bool) {
	v, ok := o.Values[key]
	return v, ok
}

// Set sets key, appending it if new.
func (o *Object) Set(key string, v any) {
	if o.Values == nil {
		o.Values = map[string]any{}
	}
	if _, ok := o.Values[key]; !ok {
		o.Keys = append(o.Keys, key)
	}
	o.Values[key] = v
}

// ParseObject parses a JSON object, preserving key order. Empty input is an
// empty object.
func ParseObject(data []byte) (*Object, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &Object{Values: map[string]any{}}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON object")
	}
	o, ok := v.(*Object)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return o, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &Object{Values: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("unexpected object key %v", kt)
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			_, err := dec.Token()
			return o, err
		case '[':
			a := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			_, err := dec.Token()
			return a, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	}
	return tok, nil
}

// MarshalJSON writes the object's keys in order.
func (o *Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.Keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := marshal(o.Values[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// Format renders o as indented JSON with a trailing newline.
func Format(o *Object) ([]byte, error) {
	raw, err := marshal(o)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// Command is one hook handler traits installs.
type Command struct {
	Event   string
	Matcher string // empty for events without tools (Stop)
	Command string
	Timeout int // seconds
}

// Commands returns the hooks for agent ("claude" or "codex"). extra is
// appended to each command line (e.g. "--provider systemone").
func Commands(agent, extra string) []Command {
	suffix := ""
	if extra != "" {
		suffix = " " + extra
	}
	matcher := "Write|Edit|MultiEdit"
	if agent == "codex" {
		matcher = "Edit|Write|apply_patch"
	}
	return []Command{
		{Event: "PostToolUse", Matcher: matcher, Command: "traits hook " + agent + suffix, Timeout: 15},
		{Event: "Stop", Command: "traits recheck" + suffix, Timeout: 30},
	}
}

// ours reports whether a handler's command runs traits' hook or recheck.
func ours(command string) bool {
	f := strings.Fields(command)
	for len(f) > 0 && strings.Contains(f[0], "=") { // leading VAR=value
		f = f[1:]
	}
	if len(f) < 2 {
		return false
	}
	bin := filepath.Base(strings.Trim(f[0], `'"`))
	bin = strings.TrimSuffix(bin, ".exe")
	return bin == "traits" && (f[1] == "hook" || f[1] == "recheck")
}

// Action is what merging did to one event.
type Action string

const (
	Added     Action = "added"
	Updated   Action = "updated"
	Unchanged Action = "unchanged"
)

// Merge adds or updates cmds in settings (a Claude Code settings.json or a
// Codex hooks.json, which share the "hooks" layout), leaving every other key
// and hook in place. A traits handler already present is replaced in place,
// never duplicated. It returns an action per command.
func Merge(settings *Object, cmds []Command) ([]Action, error) {
	var hooks *Object
	if v, ok := settings.Get("hooks"); ok {
		h, isObj := v.(*Object)
		if !isObj {
			return nil, errors.New(`"hooks" is not an object; not touching it`)
		}
		hooks = h
	} else {
		hooks = &Object{Values: map[string]any{}}
		settings.Set("hooks", hooks)
	}
	var actions []Action
	for _, c := range cmds {
		a, err := mergeEvent(hooks, c)
		if err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	return actions, nil
}

func handler(c Command) *Object {
	h := &Object{}
	h.Set("type", "command")
	h.Set("command", c.Command)
	h.Set("timeout", json.Number(fmt.Sprint(c.Timeout)))
	return h
}

func group(c Command) *Object {
	g := &Object{}
	if c.Matcher != "" {
		g.Set("matcher", c.Matcher)
	}
	g.Set("hooks", []any{handler(c)})
	return g
}

func mergeEvent(hooks *Object, c Command) (Action, error) {
	var groups []any
	if v, ok := hooks.Get(c.Event); ok {
		a, isArr := v.([]any)
		if !isArr {
			return "", fmt.Errorf("hooks.%s is not an array; not touching it", c.Event)
		}
		groups = a
	}
	want := group(c)
	wantJSON, _ := marshal(want)

	// Find groups holding a traits handler.
	var mine []int
	for i, g := range groups {
		if hasOurs(g) {
			mine = append(mine, i)
		}
	}
	if len(mine) == 1 {
		if got, _ := marshal(groups[mine[0]]); bytes.Equal(got, wantJSON) {
			return Unchanged, nil
		}
	}
	if len(mine) == 0 {
		hooks.Set(c.Event, append(groups, want))
		return Added, nil
	}
	// Replace: a group with only traits handlers (and the same matcher) is
	// swapped for ours; elsewhere our handlers are removed and the other
	// handlers keep their group and matcher.
	placed := false
	var out []any
	for _, g := range groups {
		obj, _ := g.(*Object)
		if obj == nil || !hasOurs(g) {
			out = append(out, g)
			continue
		}
		hs, _ := obj.Values["hooks"].([]any)
		var others []any
		for _, h := range hs {
			if !isOurs(h) {
				others = append(others, h)
			}
		}
		if len(others) > 0 {
			obj.Set("hooks", others)
			out = append(out, obj)
			continue
		}
		if !placed {
			out = append(out, want)
			placed = true
		}
	}
	if !placed {
		out = append(out, want)
	}
	hooks.Set(c.Event, out)
	return Updated, nil
}

func hasOurs(g any) bool {
	obj, ok := g.(*Object)
	if !ok {
		return false
	}
	hs, _ := obj.Values["hooks"].([]any)
	for _, h := range hs {
		if isOurs(h) {
			return true
		}
	}
	return false
}

func isOurs(h any) bool {
	obj, ok := h.(*Object)
	if !ok {
		return false
	}
	cmd, _ := obj.Values["command"].(string)
	return ours(cmd)
}

// Result is the outcome of Install.
type Result struct {
	Path    string
	Actions []Action
	Changed bool
	// Content is the file's merged content.
	Content []byte
	// Backup is the backup written before overwriting, if any.
	Backup string
}

// Install merges cmds into the settings file at path. With apply false it
// only computes the result. With apply true and a change to make, it backs
// up an existing file to path.bak-<timestamp> and writes the merged file.
func Install(path string, cmds []Command, apply bool, now time.Time) (Result, error) {
	res := Result{Path: path}
	for _, p := range []string{filepath.Dir(path), path} {
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return res, fmt.Errorf("%s is a symlink; edit its target explicitly", p)
		}
	}
	data, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	settings, err := ParseObject(data)
	if err != nil {
		return res, fmt.Errorf("%s: %w", path, err)
	}
	res.Actions, err = Merge(settings, cmds)
	if err != nil {
		return res, fmt.Errorf("%s: %w", path, err)
	}
	if res.Content, err = Format(settings); err != nil {
		return res, err
	}
	for _, a := range res.Actions {
		if a != Unchanged {
			res.Changed = true
		}
	}
	if !apply || !res.Changed {
		return res, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, err
	}
	// settings.json can hold secrets (env), so the backup and the rewrite
	// keep the original file's permissions.
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if exists {
		res.Backup = path + ".bak-" + now.Format("20060102-150405")
		if err := writeNew(res.Backup, data, mode); err != nil {
			return res, err
		}
	}
	// A fresh temp file (created 0600, then set to mode before any content
	// is written) so a leftover file cannot lend its permissions.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return res, err
	}
	if _, err := tmp.Write(res.Content); err != nil {
		tmp.Close()
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}
	return res, os.Rename(tmp.Name(), path)
}

// writeNew creates path, which must not exist, with mode and writes data.
func writeNew(path string, data []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
