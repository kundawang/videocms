package main

import (
	"bytes"
	"encoding/json"
	"html/template"
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// renderedPlayer executes a player template with the given chapter payload and
// returns the HTML. The other template fields hold deterministic fixtures so
// the only variable under test is the chapter list.
func renderedPlayer(t *testing.T, name, chaptersJSON string) string {
	t.Helper()
	tmpl, err := template.ParseGlob("views/*.html")
	if err != nil {
		t.Fatalf("ParseGlob() error = %v", err)
	}
	data := map[string]any{
		"Title": "Lecture", "Description": "d", "Thumbnail": "/t.webp",
		"UUID": "550e8400-e29b-41d4-a716-446655440000", "StreamUrl": "/s", "StreamUrlWidth": "640", "StreamUrlHeight": "360",
		"Width": 640, "Height": 360, "AspectWidth": 640, "AspectHeight": 360,
		"Qualitys": `[{"url":"/q","label":"360p","height":"360","width":"640"}]`, "Subtitles": `[]`, "Audios": `[]`,
		"AudioUUID": "", "Webhooks": `[]`, "StreamIsReady": true, "PlayerStatus": `{"state":"ready"}`,
		"PlayerBase": `"/v/x"`, "Folder": "/videos/qualitys", "AppName": "App", "BaseUrl": "http://x",
		"DownloadEnabled": false, "ContinueWatchingPopupEnabled": false,
		"Chapters": chaptersJSON,
	}
	var buffer bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buffer, name, data); err != nil {
		t.Fatalf("ExecuteTemplate(%q) error = %v", name, err)
	}
	return buffer.String()
}

var scriptBlock = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// chapterScript pulls the plain (non-module) inline script out of a rendered
// player so the chapter functions can be exercised without a network.
func chapterScript(t *testing.T, html string) string {
	t.Helper()
	var blocks []string
	for _, match := range scriptBlock.FindAllStringSubmatch(html, -1) {
		if strings.Contains(match[1], "formatChapterTime") {
			blocks = append(blocks, match[1])
		}
	}
	if len(blocks) != 1 {
		t.Fatalf("found %d chapter scripts, want 1", len(blocks))
	}
	return blocks[0]
}

// templateRegion returns the source between two markers in a template file.
func templateRegion(t *testing.T, path, start, end string) string {
	t.Helper()
	source := string(readTemplateFile(t, path))
	from := strings.Index(source, start)
	if from < 0 {
		t.Fatalf("%s missing start marker %q", path, start)
	}
	to := strings.Index(source[from:], end)
	if to < 0 {
		t.Fatalf("%s missing end marker %q", path, end)
	}
	return source[from : from+to]
}

// fakeDOM is the small browser surface the chapter code touches. It records
// element listeners and child trees so assertions read like a browser test.
const fakeDOM = `
function __noop() { return undefined; }
var __elements = {};
var __all = [];
function __el(tag) {
  var e = {
    tagName: tag, children: [], dataset: {}, attrs: {}, listeners: {}, classes: {},
    hidden: false, textContent: "", parent: null, currentTime: 0, paused: true,
    classList: {
      add: function (c) { e.classes[c] = true; },
      remove: function (c) { delete e.classes[c]; },
      toggle: function (c, on) {
        if (on === undefined) { on = !e.classes[c]; }
        if (on) { e.classes[c] = true; } else { delete e.classes[c]; }
      },
      contains: function (c) { return !!e.classes[c]; }
    },
    append: function () { for (var i = 0; i < arguments.length; i++) { arguments[i].parent = e; e.children.push(arguments[i]); } },
    appendChild: function (c) { c.parent = e; e.children.push(c); return c; },
    remove: function () { if (e.parent) { e.parent.children = e.parent.children.filter(function (c) { return c !== e; }); } },
    setAttribute: function (k, v) { e.attrs[k] = String(v); },
    getAttribute: function (k) { return e.attrs[k]; },
    addEventListener: function (n, fn) { (e.listeners[n] = e.listeners[n] || []).push(fn); },
    removeEventListener: __noop,
    contains: function () { return false; },
    play: function () { e.paused = false; return { catch: __noop }; },
    fire: function (n) { (e.listeners[n] || []).forEach(function (fn) { fn({}); }); }
  };
  Object.defineProperty(e, "innerHTML", { set: function () { e.children = []; }, get: function () { return ""; } });
  Object.defineProperty(e, "innerText", { set: function (v) { e.textContent = String(v); }, get: function () { return e.textContent; } });
  return e;
}
var __created = [];
var document = {
  getElementById: function (id) { var k = "#" + id; if (!__elements[k]) { __elements[k] = __el("div"); } return __elements[k]; },
  querySelector: function (sel) {
    if (sel.charAt(0) === ".") {
      var cls = sel.slice(1);
      var matches = __created.filter(function (x) { return x.classes[cls] && x.parent; });
      if (matches.length) { return matches[matches.length - 1]; }
    }
    if (!__elements[sel]) { __elements[sel] = __el("div"); }
    return __elements[sel];
  },
  querySelectorAll: function () { return __all; },
  createElement: function (tag) { var e = __el(tag); __created.push(e); return e; },
  addEventListener: __noop
};
var localStorage = { getItem: function () { return null; }, setItem: __noop };
var URL = { createObjectURL: function () { return "blob:x"; } };
var atob = function () { return ""; };
function decodeURI(v) { return v; }
var setInterval = __noop, clearInterval = __noop;
`

func newChapterVM(t *testing.T, chaptersJSON string) *goja.Runtime {
	t.Helper()
	vm := goja.New()
	if _, err := vm.RunString(fakeDOM); err != nil {
		t.Fatalf("fake DOM: %v", err)
	}
	if _, err := vm.RunString(`var Chapters = ` + chaptersJSON + `;`); err != nil {
		t.Fatalf("chapter data: %v", err)
	}
	return vm
}

func TestPlayerV2RendersAndSeeksToChapters(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		chapters string
		rendered int
		hidden   bool
	}{
		{
			name:     "ordered list",
			chapters: `{"revision":9,"chapters":[{"id":1,"title":"Intro","start_time":0},{"id":2,"title":"Demo","start_time":65.4},{"id":3,"title":"Wrap up","start_time":600}]}`,
			rendered: 3,
		},
		{
			name:     "no chapters",
			chapters: `{"revision":0,"chapters":[]}`,
			rendered: 0,
			hidden:   true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			html := renderedPlayer(t, "player_v2.html", testCase.chapters)
			if !strings.Contains(html, `\u0022revision\u0022`) && !strings.Contains(html, `"revision"`) {
				t.Fatal("chapter payload did not reach the player bootstrap")
			}

			vm := newChapterVM(t, testCase.chapters)
			if _, err := vm.RunString(`var player = __el("media-player");
var __toggle = document.getElementById("chapter-toggle"); __toggle.hidden = true;`); err != nil {
				t.Fatal(err)
			}
			if _, err := vm.RunString(templateRegion(t, "views/player_v2.html", "// --- Chapters ---", "// --- Setup Source ---")); err != nil {
				t.Fatalf("chapter script: %v", err)
			}
			value, err := vm.RunString(`
(function () {
  setupChapters();
  var list = document.getElementById("chapter-list");
  var toggle = document.getElementById("chapter-toggle");
  var result = {
    rendered: list.children.length,
    toggleHidden: toggle.hidden,
    titles: [],
    times: [],
    seekAt: null,
    playing: null,
    activeAfterSeek: [],
    activeAtTime: null,
    openAfterToggle: null,
    closedAfterClose: null
  };
  list.children.forEach(function (row) {
    var button = row.children[0];
    result.titles.push(button.children[1].textContent);
    result.times.push(button.children[0].textContent);
  });
  if (list.children.length > 0) {
    toggle.listeners.click[0]();
    result.openAfterToggle = document.getElementById("chapter-panel").classList.contains("open");
    document.getElementById("chapter-close").listeners.click[0]();
    result.closedAfterClose = !document.getElementById("chapter-panel").classList.contains("open");
    __all = list.children.map(function (row) { return row.children[0]; });
  }
  if (list.children.length > 1) {
    var target = list.children[1].children[0];
    target.listeners.click[0]();
    result.seekAt = player.currentTime;
    result.playing = !player.paused;
    result.activeAfterSeek = __all.filter(function (el) { return el.classList.contains("active"); }).map(function (el) { return el.dataset.chapterId; });
    player.currentTime = 601;
    player.fire("timeupdate");
    result.activeAtTime = currentChapter() ? currentChapter().title : null;
  }
  return JSON.stringify(result);
})()`)
			if err != nil {
				t.Fatalf("chapter behaviour: %v", err)
			}
			var got struct {
				Rendered         int      `json:"rendered"`
				ToggleHidden     bool     `json:"toggleHidden"`
				Titles           []string `json:"titles"`
				Times            []string `json:"times"`
				SeekAt           *float64 `json:"seekAt"`
				Playing          *bool    `json:"playing"`
				ActiveAfterSeek  []string `json:"activeAfterSeek"`
				ActiveAtTime     *string  `json:"activeAtTime"`
				OpenAfterToggle  *bool    `json:"openAfterToggle"`
				ClosedAfterClose *bool    `json:"closedAfterClose"`
			}
			if err := json.Unmarshal([]byte(value.String()), &got); err != nil {
				t.Fatal(err)
			}
			if got.Rendered != testCase.rendered {
				t.Fatalf("rendered = %d, want %d", got.Rendered, testCase.rendered)
			}
			if got.ToggleHidden != testCase.hidden {
				t.Fatalf("toggle hidden = %v, want %v", got.ToggleHidden, testCase.hidden)
			}
			if testCase.rendered == 0 {
				return
			}
			if strings.Join(got.Titles, "|") != "Intro|Demo|Wrap up" {
				t.Fatalf("titles = %v", got.Titles)
			}
			if strings.Join(got.Times, "|") != "0:00|1:05|10:00" {
				t.Fatalf("times = %v", got.Times)
			}
			if got.SeekAt == nil || *got.SeekAt != 65.4 {
				t.Fatalf("seek position = %v, want 65.4", got.SeekAt)
			}
			if got.Playing == nil || !*got.Playing {
				t.Fatal("clicking a chapter did not start playback")
			}
			if strings.Join(got.ActiveAfterSeek, "|") != "2" {
				t.Fatalf("active chapter after seek = %v", got.ActiveAfterSeek)
			}
			if got.ActiveAtTime == nil || *got.ActiveAtTime != "Wrap up" {
				t.Fatalf("chapter at 601s = %v", got.ActiveAtTime)
			}
			if got.OpenAfterToggle == nil || !*got.OpenAfterToggle || got.ClosedAfterClose == nil || !*got.ClosedAfterClose {
				t.Fatalf("panel toggle = open:%v closed:%v", got.OpenAfterToggle, got.ClosedAfterClose)
			}
		})
	}
}

func TestPlayerV1RendersAndSeeksToChapters(t *testing.T) {
	chapters := `{"revision":9,"chapters":[{"id":1,"title":"Intro","start_time":0},{"id":2,"title":"Demo","start_time":65.4},{"id":3,"title":"Wrap up","start_time":600}]}`
	html := renderedPlayer(t, "player.html", chapters)
	if !strings.Contains(html, "chapterListContainer") {
		t.Fatal("player.html did not render the chapter container styles")
	}

	vm := newChapterVM(t, chapters)
	if _, err := vm.RunString(`var active_video_el = __el("video");
var videoPlayerContainer = document.querySelector(".videoPlayer");`); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(templateRegion(t, "views/player.html", "// --- Chapters ---", "const videoPlayerContainer")); err != nil {
		t.Fatalf("chapter script: %v", err)
	}
	value, err := vm.RunString(`
(function () {
  renderChapterList();
  var container = document.querySelector(".chapterListContainer");
  var content = document.querySelector(".settingsContent");
  var result = {
    rendered: content.children.length,
    hidden: container.classList.contains("hidden"),
    titles: [],
    times: [],
    seekAt: null,
    playing: null,
    activeAtSeek: [],
    activeAtTime: []
  };
  content.children.forEach(function (item) {
    result.titles.push(item.children[1].textContent);
    result.times.push(item.children[0].textContent);
  });
  toggleChapterList();
  result.shown = !container.classList.contains("hidden");
  toggleChapterList();
  result.hiddenAgain = container.classList.contains("hidden");
  __all = content.children;
  content.children[1].onclick();
  result.seekAt = active_video_el.currentTime;
  result.playing = !active_video_el.paused;
  result.activeAtSeek = content.children.filter(function (el) { return el.classList.contains("active"); }).map(function (el) { return el.dataset.chapterId; });
  active_video_el.currentTime = 10;
  updateActiveChapter();
  result.activeAtTime = content.children.filter(function (el) { return el.classList.contains("active"); }).map(function (el) { return el.dataset.chapterId; });
  return JSON.stringify(result);
})()`)
	if err != nil {
		t.Fatalf("chapter behaviour: %v", err)
	}
	var got struct {
		Rendered     int      `json:"rendered"`
		Hidden       bool     `json:"hidden"`
		Titles       []string `json:"titles"`
		Times        []string `json:"times"`
		SeekAt       *float64 `json:"seekAt"`
		Playing      *bool    `json:"playing"`
		Shown        bool     `json:"shown"`
		HiddenAgain  bool     `json:"hiddenAgain"`
		ActiveAtSeek []string `json:"activeAtSeek"`
		ActiveAtTime []string `json:"activeAtTime"`
	}
	if err := json.Unmarshal([]byte(value.String()), &got); err != nil {
		t.Fatal(err)
	}
	if got.Rendered != 3 || !got.Hidden || !got.Shown || !got.HiddenAgain {
		t.Fatalf("chapter list state = %#v", got)
	}
	if strings.Join(got.Titles, "|") != "Intro|Demo|Wrap up" || strings.Join(got.Times, "|") != "0:00|1:05|10:00" {
		t.Fatalf("titles=%v times=%v", got.Titles, got.Times)
	}
	if got.SeekAt == nil || *got.SeekAt != 65.4 || got.Playing == nil || !*got.Playing {
		t.Fatalf("seek = %v playing=%v", got.SeekAt, got.Playing)
	}
	if strings.Join(got.ActiveAtSeek, "|") != "2" {
		t.Fatalf("active at seek = %v", got.ActiveAtSeek)
	}
	if strings.Join(got.ActiveAtTime, "|") != "1" {
		t.Fatalf("active at 10s = %v", got.ActiveAtTime)
	}
}

// TestPlayerWithoutChaptersKeepsOriginalPlayback asserts that a video which
// never had chapters renders and boots exactly as before: the chapter toggle
// stays hidden, no rows are produced, and the media source is untouched.
func TestPlayerWithoutChaptersKeepsOriginalPlayback(t *testing.T) {
	for _, name := range []string{"player_v2.html", "player.html"} {
		html := renderedPlayer(t, name, `{"revision":0,"chapters":[]}`)
		if name == "player_v2.html" && !strings.Contains(html, "stream/multi/master.m3u8") {
			t.Fatalf("%s lost its media source", name)
		}
		if name == "player.html" && !strings.Contains(html, "stream/muted/master.m3u8") {
			t.Fatalf("%s lost its media source", name)
		}
		if strings.Contains(html, `"start_time":`) {
			t.Fatalf("%s rendered unexpected chapter data", name)
		}
	}
	vm := newChapterVM(t, `{"revision":0,"chapters":[]}`)
	if _, err := vm.RunString(`var player = __el("media-player");
var __toggle = document.getElementById("chapter-toggle"); __toggle.hidden = true;`); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(templateRegion(t, "views/player_v2.html", "// --- Chapters ---", "// --- Setup Source ---")); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`setupChapters()`); err != nil {
		t.Fatalf("setupChapters() with no chapters: %v", err)
	}
	hidden, err := vm.RunString(`document.getElementById("chapter-toggle").hidden`)
	if err != nil || !hidden.ToBoolean() {
		t.Fatalf("toggle hidden = %v, err=%v", hidden, err)
	}
}

func TestChapterTimesAreFormattedConsistently(t *testing.T) {
	vm := newChapterVM(t, `{"revision":0,"chapters":[]}`)
	if _, err := vm.RunString(`var player = __el("media-player");`); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(templateRegion(t, "views/player_v2.html", "// --- Chapters ---", "// --- Setup Source ---")); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"0", "0:00"}, {"9.9", "0:09"}, {"60", "1:00"}, {"3599", "59:59"}, {"3600", "60:00"}, {"-3", "0:00"}} {
		value, err := vm.RunString(`formatChapterTime(` + pair[0] + `)`)
		if err != nil {
			t.Fatal(err)
		}
		if got := value.String(); got != pair[1] {
			t.Fatalf("formatChapterTime(%s) = %q, want %q", pair[0], got, pair[1])
		}
	}
}
