package jschallenge

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv(ChildEnv) != "" {
		os.Exit(ChildMain(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

// Page builds a check page shaped like the one Reddit serves: an inline
// script fills the hidden form on DOMContentLoaded and submits it.
func Page(script, action string) []byte {
	return []byte(`<!DOCTYPE html><html><head><title>Reddit</title><script>` + script + `</script></head><body>
<form hidden method="GET" action="` + action + `"><input type="hidden" name="solution" />
<input type="hidden" name="js_challenge" value="1"/><input type="hidden" name="jsc_token" value="tok123"/>
<input type="hidden" name="jsc_orig_r" value=""/></form></body></html>`)
}

// redditLike is the logic of the check observed on Reddit: the solution is
// a function of a seed, and the submit handler copies the page's query.
func redditLike(fn, seed string) string {
	return `document.addEventListener("DOMContentLoaded",async function(){var e=document.forms[0],n=(e.onsubmit=function(t){` +
		`return new URLSearchParams(document.location.search).forEach((e,n)=>t.target.appendChild(Object.assign(` +
		`document.createElement("input"),{name:n,type:"hidden",value:e}))),!0},await(` + fn + `)("` + seed + `"));` +
		`e.elements.namedItem("solution").value=n,e.requestSubmit()},{once:!0});`
}

func solve(t *testing.T, script, action string) (string, error) {
	t.Helper()
	page, _ := url.Parse("https://www.reddit.com/r/example/new/?feedViewType=compactView")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return Solver{Exe: os.Args[0], UserAgent: "Mozilla/5.0 test"}.Solve(ctx, Page(script, action), page)
}

func TestSolve(t *testing.T) {
	// The check as observed, and the same check with another rule.
	for fn, solution := range map[string]string{
		`async e=>e+e`: "abcabc",
		`async e=>[...e].reverse().join("")+btoa(e)+navigator.webdriver`: "cbaYWJjfalse",
	} {
		got, err := solve(t, redditLike(fn, "abc"), "/r/example/new/")
		want := "https://www.reddit.com/r/example/new/?solution=" + solution + "&js_challenge=1&jsc_token=tok123&jsc_orig_r=&feedViewType=compactView"
		if err != nil || got != want {
			t.Errorf("%s:\n got %s (%v)\nwant %s", fn, got, err, want)
		}
	}
	// A proof of work with SHA-256: a nonce whose hash starts with 00.
	pow := `async e=>{for(let i=0;;i++){const h=new Uint8Array(await crypto.subtle.digest("SHA-256",new TextEncoder().encode(e+i)));if(h[0]===0)return String(i)}}`
	got, err := solve(t, redditLike(pow, "abc"), "/r/example/new/")
	if err != nil || !strings.Contains(got, "solution=") || strings.Contains(got, "solution=&") {
		t.Fatalf("proof of work: %s %v", got, err)
	}
}

func TestSolveRefuses(t *testing.T) {
	for name, c := range map[string]struct{ script, action, err string }{
		"endless":     {`while(true){}`, "/r/example/new/", "time limit"},
		"memory":      {`let s="x";for(;;)s+=s;`, "/r/example/new/", "failed"},
		"no submit":   {`document.addEventListener("DOMContentLoaded",function(){})`, "/r/example/new/", "did not submit"},
		"other host":  {redditLike(`async e=>e`, "a"), "https://example.com/collect", "another site"},
		"network":     {redditLike(`async e=>(await fetch("https://example.com/")).status`, "a"), "/r/example/new/", "did not submit"},
		"file system": {`require("fs")`, "/r/example/new/", "failed"},
	} {
		start := time.Now()
		got, err := solve(t, c.script, c.action)
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %q, %v (want an error with %q)", name, got, err, c.err)
		}
		if d := time.Since(start); d > 15*time.Second {
			t.Errorf("%s took %s", name, d)
		}
	}
}

func TestExtract(t *testing.T) {
	page, _ := url.Parse("https://www.reddit.com/r/example/new/?a=1")
	in, err := Extract(Page(`var x=1;`, "/r/example/new/"), page)
	if err != nil || len(in.Scripts) != 1 || in.Form.Method != "GET" || len(in.Form.Inputs) != 4 || in.Search != "?a=1" ||
		in.Form.Inputs[2].Value != "tok123" {
		t.Fatalf("%+v %v", in, err)
	}
	if _, err := Extract([]byte(`<html><body><p>no form</p></body></html>`), page); err == nil {
		t.Fatal("a page without a form")
	}
}
