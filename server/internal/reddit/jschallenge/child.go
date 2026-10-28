package jschallenge

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// prelude stands in for the parts of a browser a check script uses: the
// document with its form, location, navigator, URLSearchParams, timers,
// base64, TextEncoder and crypto.subtle.digest (for proof-of-work checks).
// It has no network, storage or file access.
const prelude = `"use strict";
(function (input, submit) {
  var listeners = [], timers = [];
  function field(f) { return { name: f.name, value: f.value, type: f.type || "hidden" }; }
  var form = {
    method: input.form.method, action: input.form.action, onsubmit: null, _inputs: input.form.inputs.map(field),
    appendChild: function (el) { form._inputs.push(el); return el; },
    append: function (el) { form._inputs.push(el); },
    submit: function () { submit(form._inputs.map(function (i) { return { name: String(i.name || ""), value: String(i.value == null ? "" : i.value) }; })); },
    requestSubmit: function () {
      var prevented = false;
      var ev = { target: form, currentTarget: form, preventDefault: function () { prevented = true; } };
      if (typeof form.onsubmit === "function" && form.onsubmit.call(form, ev) === false) { prevented = true; }
      for (var l of listeners) { if (l.target === form && l.type === "submit") { l.fn.call(form, ev); } }
      if (!prevented) { form.submit(); }
    },
    addEventListener: function (type, fn) { listeners.push({ target: form, type: type, fn: fn }); },
  };
  form.elements = {
    namedItem: function (n) { for (var i of form._inputs) { if (i.name === n) { return i; } } return null; },
  };
  for (var i of form._inputs) { form.elements[i.name] = i; }
  var location = { href: input.href, search: input.search, pathname: input.pathname };
  function URLSearchParams(s) {
    this._p = [];
    s = String(s == null ? "" : s);
    if (s.charAt(0) === "?") { s = s.slice(1); }
    for (var part of s.split("&")) {
      if (part === "") { continue; }
      var eq = part.indexOf("=");
      var k = eq < 0 ? part : part.slice(0, eq), v = eq < 0 ? "" : part.slice(eq + 1);
      this._p.push([decodeURIComponent(k.replace(/\+/g, " ")), decodeURIComponent(v.replace(/\+/g, " "))]);
    }
  }
  URLSearchParams.prototype.forEach = function (cb) { for (var p of this._p) { cb(p[1], p[0], this); } };
  URLSearchParams.prototype.get = function (k) { for (var p of this._p) { if (p[0] === k) { return p[1]; } } return null; };
  URLSearchParams.prototype.has = function (k) { return this.get(k) !== null; };
  URLSearchParams.prototype.append = function (k, v) { this._p.push([String(k), String(v)]); };
  URLSearchParams.prototype.set = function (k, v) { this._p = this._p.filter(function (p) { return p[0] !== k; }); this.append(k, v); };
  URLSearchParams.prototype.toString = function () {
    return this._p.map(function (p) { return encodeURIComponent(p[0]) + "=" + encodeURIComponent(p[1]); }).join("&");
  };
  var document = {
    readyState: "loading", forms: [form], location: location,
    addEventListener: function (type, fn) { listeners.push({ target: document, type: type, fn: fn }); },
    removeEventListener: function () {},
    createElement: function (tag) { return { tagName: String(tag).toUpperCase() }; },
    querySelector: function (sel) { return /form/i.test(sel) ? form : null; },
    getElementById: function () { return null; },
    getElementsByTagName: function (t) { return /form/i.test(t) ? [form] : []; },
  };
  function TextEncoder() {}
  TextEncoder.prototype.encode = function (s) { return new Uint8Array(__utf8(String(s == null ? "" : s))); };
  globalThis.TextEncoder = TextEncoder;
  globalThis.crypto = {
    subtle: { digest: function (alg, data) {
      return new Promise(function (resolve) { resolve(__digest(typeof alg === "string" ? alg : alg.name, data)); });
    } },
    getRandomValues: function (a) { for (var i = 0; i < a.length; i++) { a[i] = Math.floor(Math.random() * 256); } return a; },
  };
  globalThis.navigator = { userAgent: input.userAgent, language: "en-US", languages: ["en-US", "en"], webdriver: false };
  globalThis.performance = { now: function () { return Date.now(); } };
  globalThis.document = document;
  globalThis.window = globalThis;
  globalThis.self = globalThis;
  globalThis.location = location;
  globalThis.URLSearchParams = URLSearchParams;
  globalThis.setTimeout = function (fn) { timers.push(fn); return timers.length; };
  globalThis.clearTimeout = function () {};
  globalThis.__fire = function () {
    document.readyState = "interactive";
    for (var l of listeners.slice()) { if (l.target === document && l.type === "DOMContentLoaded") { l.fn.call(document, { type: l.type }); } }
    document.readyState = "complete";
    for (var l of listeners.slice()) { if (l.target === document && l.type === "load") { l.fn.call(document, { type: l.type }); } }
    for (var l of listeners.slice()) { if (l.target === window && l.type === "load") { l.fn.call(window, { type: l.type }); } }
  };
  globalThis.addEventListener = function (type, fn) { listeners.push({ target: window, type: type, fn: fn }); };
  globalThis.__timers = function () { var t = timers; timers = []; for (var fn of t) { if (typeof fn === "function") { fn(); } } return t.length; };
})(__input, __submit);
`

// ChildMain runs one check: Input as JSON on r, Output as JSON on w. It
// returns the exit code.
func ChildMain(r io.Reader, w io.Writer) int {
	guardMemory()
	data, err := io.ReadAll(io.LimitReader(r, 2*maxScript+(64<<10)))
	if err != nil {
		return fail(err)
	}
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		return fail(err)
	}
	out, err := run(&in)
	if err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		return fail(err)
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, err)
	return 2
}

func run(in *Input) (*Output, error) {
	vm := goja.New()
	vm.SetMaxCallStackSize(1000)
	timer := time.AfterFunc(scriptLimit, func() { vm.Interrupt("time limit") })
	defer timer.Stop()
	out := &Output{}
	if err := vm.Set("__input", in.forJS()); err != nil {
		return nil, err
	}
	if err := vm.Set("__submit", func(fields []map[string]any) {
		if out.Submitted {
			return
		}
		out.Submitted = true
		for _, f := range fields {
			out.Fields = append(out.Fields, Field{Name: fmt.Sprint(f["name"]), Value: fmt.Sprint(f["value"])})
		}
	}); err != nil {
		return nil, err
	}
	if err := natives(vm); err != nil {
		return nil, err
	}
	if _, err := vm.RunString(prelude); err != nil {
		return nil, fmt.Errorf("prelude: %w", err)
	}
	for _, s := range in.Scripts {
		if _, err := vm.RunString(s); err != nil {
			return nil, fmt.Errorf("script: %w", err)
		}
	}
	if _, err := vm.RunString("__fire()"); err != nil {
		return nil, fmt.Errorf("script: %w", err)
	}
	// Timers run after the page's own handlers, a bounded number of rounds.
	for i := 0; i < 20 && !out.Submitted; i++ {
		n, err := vm.RunString("__timers()")
		if err != nil {
			return nil, fmt.Errorf("script: %w", err)
		}
		if n.ToInteger() == 0 {
			break
		}
	}
	return out, nil
}

// natives are the prelude's helpers written in Go: base64, UTF-8 and
// digests.
func natives(vm *goja.Runtime) error {
	latin1 := func(s string) []byte { // btoa works on the code units of a "binary string"
		b := make([]byte, 0, len(s))
		for _, r := range s {
			if r > 0xff {
				panic(vm.NewTypeError("btoa: character out of range"))
			}
			b = append(b, byte(r))
		}
		return b
	}
	for name, fn := range map[string]any{
		"btoa": func(s string) string { return base64.StdEncoding.EncodeToString(latin1(s)) },
		"atob": func(s string) string {
			b, err := base64.StdEncoding.DecodeString(strings.TrimRight(s, "=") + strings.Repeat("=", (4-len(strings.TrimRight(s, "="))%4)%4))
			if err != nil {
				panic(vm.NewTypeError("atob: invalid base64"))
			}
			r := make([]rune, len(b))
			for i, c := range b {
				r[i] = rune(c)
			}
			return string(r)
		},
		"__utf8": func(s string) goja.ArrayBuffer { return vm.NewArrayBuffer([]byte(s)) },
		"__digest": func(alg string, data goja.Value) goja.ArrayBuffer {
			var b []byte
			switch v := data.Export().(type) {
			case goja.ArrayBuffer:
				b = v.Bytes()
			case []byte:
				b = v
			case string:
				b = []byte(v)
			default:
				panic(vm.NewTypeError("digest: unsupported data"))
			}
			var h hash.Hash
			switch strings.ToUpper(alg) {
			case "SHA-1":
				h = sha1.New()
			case "SHA-256":
				h = sha256.New()
			case "SHA-384":
				h = sha512.New384()
			case "SHA-512":
				h = sha512.New()
			default:
				panic(vm.NewTypeError("digest: unsupported algorithm " + alg))
			}
			h.Write(b)
			return vm.NewArrayBuffer(h.Sum(nil))
		},
	} {
		if err := vm.Set(name, fn); err != nil {
			return err
		}
	}
	return nil
}

// forJS hands the input to goja as plain values.
func (in *Input) forJS() map[string]any {
	inputs := make([]any, len(in.Form.Inputs))
	for i, f := range in.Form.Inputs {
		inputs[i] = map[string]any{"name": f.Name, "value": f.Value, "type": f.Type}
	}
	return map[string]any{"href": in.Href, "search": in.Search, "pathname": in.Pathname, "userAgent": in.UserAgent,
		"form": map[string]any{"method": in.Form.Method, "action": in.Form.Action, "inputs": inputs}}
}

// guardMemory ends the child when its heap passes the limit: goja itself
// has no memory limit, and a script can grow a string without end.
func guardMemory() {
	debug.SetMemoryLimit(memoryLimit / 2)
	go func() {
		var ms runtime.MemStats
		for {
			time.Sleep(20 * time.Millisecond)
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > memoryLimit {
				fmt.Fprintln(os.Stderr, "memory limit")
				os.Exit(3)
			}
		}
	}()
}
