package main

import (
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
)

var joinTmpl = template.Must(template.New("join").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Central clipboard</title>
<style>
:root{color-scheme:light dark;--bg:#fff;--fg:#1c1c1e;--mut:#6b6b70;--card:#f2f2f7;--acc:#0a84ff}
@media (prefers-color-scheme:dark){:root{--bg:#000;--fg:#f2f2f7;--mut:#9a9aa0;--card:#1c1c1e}}
body{font:17px/1.5 -apple-system,system-ui,sans-serif;background:var(--bg);color:var(--fg);margin:0;padding:20px 16px 64px}
main{max-width:560px;margin:0 auto}
h1{font-size:26px;margin:0 0 4px} h2{font-size:20px;margin:32px 0 8px}
.mut{color:var(--mut);font-size:15px}
section{background:var(--card);border-radius:14px;padding:4px 16px 16px;margin-top:20px}
label{display:block;margin-top:12px;font-size:15px;color:var(--mut)}
input{width:100%;box-sizing:border-box;font:inherit;padding:12px;border-radius:10px;border:1px solid var(--mut);background:var(--bg);color:var(--fg)}
button,.btn{display:block;width:100%;box-sizing:border-box;margin-top:12px;padding:13px;border:0;border-radius:10px;background:var(--acc);color:#fff;font:inherit;text-align:center;text-decoration:none}
pre{overflow-x:auto;background:var(--bg);border-radius:10px;padding:12px;font-size:14px}
ol{padding-left:22px} li{margin:10px 0}
#msg{color:#d70015;min-height:1.5em}
</style>
<main>
<h1>Central clipboard</h1>
<p class="mut">Hub at {{.Hub}}. Joining needs the passphrase from whoever runs the hub.</p>

<section>
<h2>iPhone</h2>
<form id="join">
<label for="name">Device name</label>
<input id="name" value="iphone" autocapitalize="off" autocorrect="off" spellcheck="false">
<label for="pass">Passphrase</label>
<input id="pass" type="password" autocomplete="off" autocapitalize="off" autocorrect="off">
<button type="submit">Join</button>
<p id="msg"></p>
</form>
<div id="done" hidden>
<ol>
<li>Copy your URL. It contains this phone's token, so keep it private.
<input id="url" readonly><button type="button" id="copy">Copy URL</button></li>
{{if .Single}}<li>Add the shortcut. Tap <b>Add to Shortcuts</b>. Safari asks to open Shortcuts: tap <b>Open</b>. When it asks for the hub URL, press and hold the field, tap <b>Paste</b>, then tap <b>Add Shortcut</b>.
<a class="btn" href="{{.Import}}">Add to Shortcuts</a>
<span class="mut">Nothing happened? <a href="/shortcuts/Clipboard.shortcut">Download the file</a> instead, then open it from Safari's downloads (the arrow icon) or the Files app.</span></li>
<li>Use it: run <b>Clipboard</b> and choose <b>Send</b> to put this phone's clipboard on your other machines, or <b>Get</b> to bring theirs here.
Put it on the Action Button or Back Tap for one-touch use.</li>{{else}}<li>Add both shortcuts. Tap each button and open the download from Safari's downloads (the arrow icon) or the Files app. When the Shortcuts app asks for the URL, paste it, then tap <b>Add Shortcut</b>.
<a class="btn" href="/shortcuts/Send%20to%20Computer.shortcut">Send to Computer</a>
<a class="btn" href="/shortcuts/Get%20from%20Computer.shortcut">Get from Computer</a></li>
<li>Use them: run <b>Send to Computer</b> after copying on the phone, and <b>Get from Computer</b> to bring your other machines' clipboard to the phone.
Tip: a new shortcut with <i>Choose from Menu</i> and two <i>Run Shortcut</i> actions gives you one entry for the Action Button or Back Tap.</li>{{end}}
</ol>
</div>
</section>

<section>
<h2>Linux (Wayland)</h2>
<pre>curl -fsSL {{.Hub}}/install.sh | bash</pre>
<p class="mut">Asks for the passphrase, installs the agent as a systemd user service and starts it. Needs curl and wl-clipboard (ImageMagick converts phone photos). Unattended:</p>
<pre>curl -fsSL {{.Hub}}/install.sh | CLIP_PASSPHRASE='…' CLIP_NAME=laptop2 bash</pre>
</section>

<section>
<h2>Scripts and agents</h2>
<pre>curl -s -X POST {{.Hub}}/enroll \
  -d name=my-device -d passphrase='…'</pre>
<p class="mut">Returns <code>{"name","token","url"}</code>. Send the token as <code>X-Token</code> or <code>?t=</code>.</p>
</section>
</main>
<script>
const $ = id => document.getElementById(id);
$('join').addEventListener('submit', async e => {
  e.preventDefault();
  $('msg').textContent = '';
  try {
    const r = await fetch('/enroll', {method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({name: $('name').value.trim(), passphrase: $('pass').value})});
    if (!r.ok) {
      $('msg').textContent = ({403: 'Wrong passphrase.', 409: 'That name is taken.', 404: 'Joining is disabled on this hub.',
        429: 'Too many wrong passphrases. Wait a few minutes.', 400: 'Use 1-32 letters, digits, - or _ for the name.'})[r.status] || 'Error ' + r.status;
      return;
    }
    const d = await r.json();
    $('url').value = d.url;
    $('join').hidden = true;
    $('done').hidden = false;
  } catch (err) {
    $('msg').textContent = 'Could not reach the hub: ' + err;
  }
});
$('copy').addEventListener('click', () => {
  const u = $('url');
  u.select(); u.setSelectionRange(0, u.value.length);
  document.execCommand('copy');
  $('copy').textContent = 'Copied';
});
</script>
</html>
`))

func (s *Server) joinPage(w *respWriter, r *http.Request) {
	hub, ok := s.baseURL(r)
	if !ok {
		http.Error(w, "bad Host header", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, err := fs.Stat(s.files, "shortcuts/Clipboard.shortcut") // our own menu shortcut, once it has been signed
	// Apple's link that opens the Shortcuts app and imports from a URL. We build it from the
	// address the client used, which is checked above, so it is safe to mark as a URL.
	importLink := "shortcuts://import-shortcut?url=" + url.QueryEscape(hub+"/shortcuts/Clipboard.shortcut") + "&name=Clipboard"
	joinTmpl.Execute(w, struct {
		Hub    string
		Single bool
		Import template.URL
	}{hub, err == nil, template.URL(importLink)})
}
