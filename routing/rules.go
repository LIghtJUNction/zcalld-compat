// Package routing implements the optional Magpie rule compatibility worker.
// The condition, time-window and folder semantics are derived from
// yetone/magpie, MIT, commit 1e174f8f9f613a920203a4658d259456fcab5d0d.
// See NOTICE_ROUTING.md. Requests reaching this package have already passed
// the gateway's privacy hooks; this package never opens an upstream socket.
package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode/utf8"
)

var efforts = []string{"low", "medium", "high", "xhigh", "max", "ultra"}
var weekdays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

type Window struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Days     []string `json:"days,omitempty"`
	Zone     string   `json:"zone,omitempty"`
	from, to int
	location *time.Location
}
type Condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value,omitempty"`
	re    *regexp.Regexp
}
type Rule struct {
	Use           string      `json:"use"`
	More          []string    `json:"more,omitempty"`
	UseEffort     string      `json:"useEffort,omitempty"`
	Tokens        int64       `json:"tokens,omitempty"`
	Images        bool        `json:"images,omitempty"`
	Effort        string      `json:"effort,omitempty"`
	Agents        []string    `json:"agents,omitempty"`
	Intent        string      `json:"intent,omitempty"`
	Compact       bool        `json:"compact,omitempty"`
	Time          *Window     `json:"time,omitempty"`
	Folders       []string    `json:"folders,omitempty"`
	ExceptFolders []string    `json:"exceptFolders,omitempty"`
	Where         []Condition `json:"where,omitempty"`
	Pause         bool        `json:"pause,omitempty"`
}
type Group struct {
	Name  string `json:"name"`
	Rules []Rule `json:"rules"`
}
type Configuration struct {
	Groups []Group `json:"groups"`
}
type Engine struct {
	groups map[string][]Rule
}
type Facts struct {
	Tokens                                   int64
	Images, Thinking, Off, Subagent, Compact bool
	Effort, Agent, Message, Folder           string
	System, Last                             []string
	At                                       time.Time
}

var conditionOps = map[string][]string{
	"tokens":   {"gte", "gt", "lt", "lte"},
	"effort":   {"is", "not", "gte", "lte"},
	"agent":    {"is", "not"},
	"subagent": {"is", "not"},
	"message":  {"contains", "excludes", "starts", "matches"},
	"system":   {"contains", "excludes", "starts", "matches"},
	"last":     {"contains", "excludes", "starts", "matches"},
}

func strictJSON(data []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return errors.New("invalid routing JSON schema")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("extra routing JSON input")
	}
	return nil
}
func New(data []byte) (*Engine, error) {
	var c Configuration
	if len(data) > 8<<20 || strictJSON(data, &c) != nil || len(c.Groups) > 1024 {
		return nil, errors.New("invalid or oversized routing configuration")
	}
	engine := &Engine{groups: map[string][]Rule{}}
	total := 0
	for _, group := range c.Groups {
		if group.Name == "" || len(group.Name) > 512 {
			return nil, errors.New("invalid routing group name")
		}
		if _, exists := engine.groups[group.Name]; exists {
			return nil, errors.New("duplicate routing group")
		}
		total += len(group.Rules)
		if len(group.Rules) > 1024 || total > 4096 {
			return nil, errors.New("too many routing rules")
		}
		for i := range group.Rules {
			if err := group.Rules[i].clean(); err != nil {
				return nil, fmt.Errorf("rule %d: %w", i+1, err)
			}
		}
		engine.groups[group.Name] = group.Rules
	}
	return engine, nil
}
func (r *Rule) clean() error {
	if r.Use == "" || len(r.Use) > 512 || len(r.More) > 1024 {
		return errors.New("rule needs a bounded target")
	}
	if r.Tokens < 0 || r.Tokens > 1<<53 {
		return errors.New("invalid token threshold")
	}
	if r.Effort != "" && r.Effort != "on" && !slices.Contains(efforts, r.Effort) {
		return errors.New("invalid rule effort")
	}
	if r.UseEffort != "" && !slices.Contains(efforts, r.UseEffort) {
		return errors.New("invalid output effort")
	}
	if utf8.RuneCountInString(r.Intent) > 200 || len(r.Agents) > 256 || len(r.Where) > 64 {
		return errors.New("rule exceeds its condition limits")
	}
	if len(r.Folders)+len(r.ExceptFolders) > 256 {
		return errors.New("too many rule folders")
	}
	for _, folder := range append(slices.Clone(r.Folders), r.ExceptFolders...) {
		if len(folder) > 4096 || folderKey(folder, userHome(), runtime.GOOS) == "" {
			return errors.New("rule folder must be a complete path or home-relative path")
		}
	}
	if r.Time != nil {
		if err := r.Time.clean(); err != nil {
			return err
		}
	}
	for i := range r.Where {
		if err := r.Where[i].clean(); err != nil {
			return err
		}
		if r.Pause && r.Where[i].Field != "agent" {
			return errors.New("pause conditions are limited to time, agent and folder")
		}
	}
	if r.Pause && (len(r.More) > 0 || r.UseEffort != "" || r.Tokens > 0 || r.Images || r.Effort != "" || r.Intent != "" || r.Compact) {
		return errors.New("pause conditions are limited to time, agent and folder")
	}
	if r.Tokens == 0 && !r.Images && r.Effort == "" && len(r.Agents) == 0 && r.Intent == "" &&
		!r.Compact && r.Time == nil && len(r.Folders) == 0 && len(r.ExceptFolders) == 0 && len(r.Where) == 0 {
		return errors.New("rule has no conditions")
	}
	return nil
}
func clockMinutes(s string) (int, bool) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= '０' && r <= '９':
			b.WriteRune(r - '０' + '0')
		case strings.ContainsRune(":：.．hH時时点點", r):
			b.WriteByte(':')
		case r == '分':
		default:
			b.WriteRune(r)
		}
	}
	v := b.String()
	h, m, colon := strings.Cut(v, ":")
	if !colon && len(v) >= 3 {
		h, m = v[:len(v)-2], v[len(v)-2:]
	}
	m = strings.TrimSpace(m)
	if colon && m == "" || !colon && len(v) <= 2 {
		m = "00"
	}
	if len(h) < 1 || len(h) > 2 || len(m) != 2 || strings.IndexFunc(h+m, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	hh, _ := strconv.Atoi(h)
	mm, _ := strconv.Atoi(m)
	return hh*60 + mm, hh <= 23 && mm <= 59
}
func (w *Window) clean() error {
	var a, b bool
	w.from, a = clockMinutes(w.From)
	w.to, b = clockMinutes(w.To)
	if !a || !b {
		return errors.New("time window needs valid hours and minutes")
	}
	if len(w.Days) > 7 {
		return errors.New("too many days in a time window")
	}
	known := make([]string, 0, 7)
	for _, day := range w.Days {
		value := strings.ToLower(strings.TrimSpace(day))
		canonical := ""
		for d := time.Sunday; d <= time.Saturday; d++ {
			if len(value) >= 3 && strings.HasPrefix(strings.ToLower(d.String()), value) {
				canonical = weekdays[d]
			}
		}
		if canonical == "" {
			return errors.New("invalid day in time window")
		}
		if !slices.Contains(known, canonical) {
			known = append(known, canonical)
		}
	}
	if len(known) == 7 {
		known = nil
	}
	w.Days = known
	if w.from == w.to && len(known) == 0 {
		return errors.New("time window is every hour of every day")
	}
	w.Zone = strings.TrimSpace(w.Zone)
	if w.Zone == "" {
		w.location = time.Local
	} else {
		var err error
		w.location, err = time.LoadLocation(w.Zone)
		if err != nil {
			return errors.New("unknown IANA time zone")
		}
	}
	return nil
}
func (w Window) holds(at time.Time) bool {
	if at.IsZero() || w.location == nil {
		return false
	}
	at = at.In(w.location)
	minute, day := at.Hour()*60+at.Minute(), at.Weekday()
	switch {
	case w.from == w.to:
	case w.from < w.to:
		if minute < w.from || minute >= w.to {
			return false
		}
	default:
		if minute >= w.to && minute < w.from {
			return false
		}
		if minute < w.to {
			day = (day + 6) % 7
		}
	}
	return len(w.Days) == 0 || slices.Contains(w.Days, weekdays[day])
}
func (c *Condition) clean() error {
	c.Field = strings.ToLower(strings.TrimSpace(c.Field))
	c.Op = strings.ToLower(strings.TrimSpace(c.Op))
	if !slices.Contains(conditionOps[c.Field], c.Op) {
		return errors.New("unknown rule condition field or operation")
	}
	switch c.Field {
	case "tokens":
		value := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(c.Value, "_", "")))
		multiplier := 1.0
		if strings.HasSuffix(value, "k") {
			multiplier, value = 1e3, strings.TrimSuffix(value, "k")
		} else if strings.HasSuffix(value, "m") {
			multiplier, value = 1e6, strings.TrimSuffix(value, "m")
		}
		number, err := strconv.ParseFloat(value, 64)
		number *= multiplier
		if err != nil || number < 0 || number > 1<<53 || math.IsNaN(number) || math.IsInf(number, 0) {
			return errors.New("condition token threshold must be a bounded nonnegative length")
		}
		c.Value = strconv.FormatInt(int64(number), 10)
	case "effort":
		c.Value = strings.ToLower(strings.TrimSpace(c.Value))
		level := slices.Contains(efforts, c.Value)
		if (!level && c.Value != "on" && c.Value != "off") || (c.Op == "gte" || c.Op == "lte") && !level {
			return errors.New("invalid condition effort")
		}
	case "agent":
		values := strings.FieldsFunc(strings.ToLower(c.Value), func(r rune) bool { return r == ',' || r == '|' || r == ' ' })
		if len(values) == 0 || len(c.Value) > 4096 {
			return errors.New("agent condition needs a bounded list")
		}
		c.Value = strings.Join(values, ",")
	case "subagent":
		c.Value = ""
	default:
		c.Value = strings.TrimSpace(c.Value)
		if c.Value == "" || utf8.RuneCountInString(c.Value) > 500 {
			return errors.New("text condition needs 1 through 500 characters")
		}
		if c.Op == "matches" {
			if v := c.Value; len(v) > 2 && v[0] == '/' && v[len(v)-1] == '/' {
				c.Value = v[1 : len(v)-1]
			}
			var err error
			c.re, err = regexp.Compile("(?i)" + c.Value)
			if err != nil {
				return errors.New("invalid Go RE2 regular expression")
			}
		}
	}
	return nil
}
func (c Condition) holds(q Facts) bool {
	switch c.Field {
	case "tokens":
		n, _ := strconv.ParseInt(c.Value, 10, 64)
		switch c.Op {
		case "gte":
			return q.Tokens >= n
		case "gt":
			return q.Tokens > n
		case "lt":
			return q.Tokens < n
		case "lte":
			return q.Tokens <= n
		}
	case "effort":
		off := q.Off || !q.Thinking && q.Effort == ""
		is := !off && q.Effort == c.Value
		if c.Value == "off" {
			is = off
		}
		if c.Value == "on" {
			is = !off
		}
		have, want := slices.Index(efforts, q.Effort), slices.Index(efforts, c.Value)
		switch c.Op {
		case "is":
			return is
		case "not":
			return !is
		case "gte":
			return !off && want >= 0 && have >= want
		case "lte":
			return want >= 0 && (off || have >= 0 && have <= want)
		}
	case "agent":
		is := slices.Contains(strings.Split(c.Value, ","), strings.ToLower(q.Agent))
		return is == (c.Op == "is")
	case "subagent":
		return q.Subagent == (c.Op == "is")
	case "message":
		return c.holdsIn([]string{q.Message})
	case "system":
		return c.holdsIn(q.System)
	case "last":
		return c.holdsIn(q.Last)
	}
	return false
}
func (c Condition) holdsIn(texts []string) bool {
	value := strings.ToLower(c.Value)
	for _, text := range texts {
		switch c.Op {
		case "contains", "excludes":
			if strings.Contains(strings.ToLower(text), value) {
				return c.Op == "contains"
			}
		case "starts":
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), strings.TrimSpace(value)) {
				return true
			}
		case "matches":
			if c.re != nil && c.re.MatchString(text) {
				return true
			}
		}
	}
	return c.Op == "excludes"
}
func (r Rule) holdsWithoutIntent(q Facts) bool {
	if r.Tokens > 0 && q.Tokens < r.Tokens || r.Images && !q.Images {
		return false
	}
	if r.Effort != "" {
		off := q.Off || !q.Thinking && q.Effort == ""
		if off {
			return false
		}
		if r.Effort != "on" && slices.Index(efforts, q.Effort) < slices.Index(efforts, r.Effort) {
			return false
		}
	}
	if len(r.Agents) > 0 && !slices.Contains(r.Agents, q.Agent) {
		return false
	}
	if r.Compact && !q.Compact || r.Time != nil && !r.Time.holds(q.At) {
		return false
	}
	in := func(roots []string) bool {
		for _, root := range roots {
			if FolderUnder(q.Folder, root, userHome(), runtime.GOOS) {
				return true
			}
		}
		return false
	}
	if len(r.Folders) > 0 && !in(r.Folders) || in(r.ExceptFolders) {
		return false
	}
	for _, c := range r.Where {
		if !c.holds(q) {
			return false
		}
	}
	return true
}
func (e *Engine) evaluate(group string, q Facts) ([]bool, error) {
	rules, ok := e.groups[group]
	if !ok {
		return nil, errors.New("unknown configured routing group")
	}
	result := make([]bool, len(rules))
	for i, rule := range rules {
		result[i] = rule.holdsWithoutIntent(q)
	}
	return result, nil
}
func (e *Engine) needsFolder(group string) bool {
	for _, rule := range e.groups[group] {
		if len(rule.Folders)+len(rule.ExceptFolders) > 0 {
			return true
		}
	}
	return false
}
func userHome() string { home, _ := os.UserHomeDir(); return home }

var drivePath = regexp.MustCompile("^[A-Za-z]:(/|$)")
var wslMount = regexp.MustCompile("^/mnt/([A-Za-z])(/|$)")
var wslShare = regexp.MustCompile("(?i)^//wsl(\\$|\\.localhost)/[^/]+(/|$)")

func folderKey(value, home, platform string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if value == "~" || strings.HasPrefix(value, "~/") {
		home = strings.ReplaceAll(home, "\\", "/")
		if home == "" {
			return ""
		}
		value = home + value[1:]
	}
	fold := platform == "windows" || platform == "darwin"
	if match := wslShare.FindString(value); match != "" {
		value = "/" + value[len(match):]
	}
	if drivePath.MatchString(value) {
		value = "/mnt/" + strings.ToLower(value[:1]) + value[2:]
	}
	if match := wslMount.FindStringSubmatch(value); match != nil {
		value = "/mnt/" + strings.ToLower(match[1]) + value[len("/mnt/")+1:]
		fold = true
	}
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	value = path.Clean(value)
	if fold {
		value = strings.ToLower(value)
	}
	return value
}
func FolderUnder(folder, root, home, platform string) bool {
	f, r := folderKey(folder, home, platform), folderKey(root, home, platform)
	return f != "" && r != "" && (f == r || r == "/" || strings.HasPrefix(f, r+"/"))
}
