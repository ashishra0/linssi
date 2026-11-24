package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Every fixture's "real content" is a magenta block (#ff00ff). Grading looks
// for it in the image, so a capture is right only if the block is visible at
// full opacity: a half-faded, skeleton or covered page doesn't count.
const markerCSS = `#marker{width:400px;height:200px;background:#ff00ff}`

var magenta = color.RGBA{255, 0, 255, 255}

const header = `<header style="padding:16px 24px;border-bottom:1px solid #ddd;font:600 18px system-ui">Acme</header>`

func page(title, css, body string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">` +
		`<title>` + title + `</title><style>body{margin:0;font:16px system-ui}main{padding:24px}` + markerCSS + css + `</style></head>` +
		`<body>` + body + `</body></html>`
}

func content(css, main string) string { return page("Dashboard", css, header+"<main>"+main+"</main>") }

const signInForm = `<main style="max-width:360px;margin:80px auto"><h1>Sign in</h1>
<form><label>Email <input type="email"></label><br><label>Password <input type="password"></label><br><button>Sign in</button></form></main>`

func fixtures() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ms := atoi(q.Get("ms"), 1000)
		h := atoi(q.Get("h"), 4000)
		html := func(s string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, s)
		}
		switch r.URL.Path {
		case "/plain":
			html(content("", `<div id="marker"></div>`))
		case "/fade":
			html(content(fmt.Sprintf(`#marker{animation:in %dms ease-out both}@keyframes in{from{opacity:0}}`, ms), `<div id="marker"></div>`))
		case "/fade-delay":
			html(content(fmt.Sprintf(`#marker{animation:in 300ms ease-out %dms both}@keyframes in{from{opacity:0}}`, ms), `<div id="marker"></div>`))
		case "/slide":
			html(content(fmt.Sprintf(`#marker{animation:in %dms ease-out both}@keyframes in{from{transform:translateX(-120vw)}}`, ms), `<div id="marker"></div>`))
		case "/late":
			html(content("", fmt.Sprintf(`<p>Welcome back.</p><script>setTimeout(()=>{const d=document.createElement('div');d.id='marker';document.querySelector('main').append(d)},%d)</script>`, ms)))
		case "/spa":
			html(page("App", `.spinner{width:40px;height:40px;border:4px solid #ccc;border-top-color:#333;border-radius:50%;animation:r 1s linear infinite}@keyframes r{to{transform:rotate(1turn)}}`,
				header+fmt.Sprintf(`<main id="root"><div class="spinner"></div></main><script>fetch('/api?ms=%d').then(r=>r.json()).then(()=>{document.getElementById('root').innerHTML='<div id="marker"></div>'})</script>`, ms)))
		case "/skeleton":
			html(page("App", `.skeleton{height:24px;margin:12px 0;border-radius:6px;background:linear-gradient(90deg,#eee,#ddd,#eee);background-size:200%;animation:sh 1.2s linear infinite}@keyframes sh{to{background-position:-200% 0}}`,
				header+fmt.Sprintf(`<main id="root"><div class="skeleton"></div><div class="skeleton"></div><div class="skeleton" style="width:60%%"></div></main><script>setTimeout(()=>{document.getElementById('root').innerHTML='<div id="marker"></div>'},%d)</script>`, ms)))
		case "/hydrate":
			html(content(`.pending #marker{visibility:hidden}`, fmt.Sprintf(`<div id="marker"></div><script>document.body.classList.add('pending');setTimeout(()=>document.body.classList.remove('pending'),%d)</script>`, ms)))
		case "/img":
			html(content("", fmt.Sprintf(`<img src="/marker.png?ms=%d" width="400" height="200">`, ms)))
		case "/bg":
			html(content(fmt.Sprintf(`.hero{width:400px;height:200px;background:url(/marker.png?ms=%d)}`, ms), `<div class="hero"></div>`))
		case "/iframe":
			html(content("iframe{width:600px;height:400px;border:0}", fmt.Sprintf(`<iframe src="/late?ms=%d"></iframe>`, ms)))
		case "/canvas":
			html(content("", fmt.Sprintf(`<canvas width="400" height="200"></canvas><script>setTimeout(()=>{const c=document.querySelector('canvas').getContext('2d');c.fillStyle='#ff00ff';c.fillRect(0,0,400,200)},%d)</script>`, ms)))
		case "/lazy":
			html(content("", fmt.Sprintf(`<div style="height:%dpx">Scroll down.</div><img loading="lazy" src="/marker.png?ms=200" width="400" height="200">`, h)))
		case "/reveal":
			html(content(`.r{opacity:0;transition:opacity .6s}.r.in{opacity:1}`, fmt.Sprintf(`<div style="height:%dpx">Scroll down.</div><div id="marker" class="r"></div>
<script>new IntersectionObserver(es=>es.forEach(e=>e.isIntersecting&&e.target.classList.add('in'))).observe(document.querySelector('.r'))</script>`, h)))
		case "/tall":
			html(content("", fmt.Sprintf(`<div style="height:%dpx;background:linear-gradient(#fff,#cde)">A long page.</div><div id="marker"></div>`, h)))
		case "/loop":
			html(content(`.c{width:200px;height:40px;background:#39f;animation:m 2s ease-in-out infinite alternate}@keyframes m{to{transform:translateX(400px)}}`, `<div id="marker"></div><div class="c"></div>`))
		case "/hang":
			html(content("", `<div id="marker"></div><script async src="/never"></script>`))
		case "/infinite":
			html(content(`.item{height:120px;margin:12px 0;background:#eef}`, `<div id="marker"></div><div id="feed"></div>
<script>const f=document.getElementById('feed');function more(){for(let i=0;i<20;i++){const d=document.createElement('div');d.className='item';f.append(d)}}more();
addEventListener('scroll',()=>{if(innerHeight+scrollY>document.body.scrollHeight-800)more()})</script>`))
		case "/banner":
			html(content(`.cb{position:fixed;left:0;right:0;bottom:0;padding:16px 24px;background:#222;color:#fff;animation:up .5s ease-out .3s both}@keyframes up{from{transform:translateY(100%)}}`,
				`<div id="marker"></div><div class="cb">We use cookies. <button>Accept</button></div>`))
		case "/consent-wall":
			html(content(`body{overflow:hidden}.ov{position:fixed;inset:0;background:rgba(0,0,0,.6);display:grid;place-items:center}.dlg{background:#fff;padding:32px;width:480px}`,
				`<div id="marker"></div><div class="ov"><div class="dlg"><h2>We value your privacy</h2><p>Accept cookies to continue.</p><button>Accept all</button> <button>Manage</button></div></div>`))
		case "/dark":
			html(content(`#marker{display:none}@media (prefers-color-scheme:dark){body{background:#111;color:#eee}#marker{display:block}}`, `<div id="marker"></div>`))
		case "/mobile":
			html(content(`#marker{display:none;width:300px}@media (max-width:500px){#marker{display:block}}`, `<div id="marker"></div>`))
		case "/account", "/login":
			html(page("Acme", "", header+signInForm))
		case "/signin-oauth":
			html(page("Welcome to Acme", "button{display:block;width:280px;margin:12px auto;padding:12px}", header+`<main style="text-align:center;margin-top:80px"><h1>Welcome</h1><button>Continue with Google</button><button>Continue with Apple</button></main>`))
		case "/signin-late":
			// mymind-style: an empty page that renders its sign-in screen from JS, then fades it in.
			html(page("Acme", `main{animation:in .6s ease-out both}@keyframes in{from{opacity:0}}`, fmt.Sprintf(`<div id="app"></div><script>setTimeout(()=>{document.getElementById('app').innerHTML=%q},%d)</script>`, signInForm, ms)))
		case "/app":
			http.Redirect(w, r, "/login?next=/app", http.StatusFound)
		case "/js-redirect":
			html(page("Loading", "", `<script>setTimeout(()=>location.replace('/login?next=/js-redirect'),300)</script>`))
		case "/status":
			code := atoi(q.Get("code"), 404)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(code)
			fmt.Fprint(w, page(fmt.Sprintf("%d %s", code, http.StatusText(code)), "", header+fmt.Sprintf(`<main><h1>%d</h1><p>%s</p></main>`, code, http.StatusText(code))))
		case "/notfound200":
			html(page("Page not found · Acme", "", header+`<main><h1>404</h1><p>Sorry, we couldn't find that page.</p></main>`))
		case "/challenge":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(atoi(q.Get("code"), 403))
			fmt.Fprint(w, page("Just a moment...", "", `<main style="text-align:center;margin-top:120px"><h1>Checking if the site connection is secure</h1><p>example.com needs to review the security of your connection before proceeding.</p><p>Ray ID: 8c1f2a3b4d5e6f70</p></main>`))
		case "/blank":
			html(page("Acme", "", ""))
		case "/blank-js":
			html(page("Acme", "", `<div id="root"></div><script>throw new Error('chunk failed to load')</script>`))
		case "/white":
			html(page("Acme", "body{color:#fff}", `<main><h1>Dashboard</h1><p>Text the theme forgot to color.</p></main>`))
		case "/api":
			sleep(r, ms)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		case "/marker.png":
			sleep(r, ms)
			w.Header().Set("Content-Type", "image/png")
			w.Write(solidPNG(atoi(q.Get("w"), 400), atoi(q.Get("h"), 200)))
		case "/never":
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	})
}

func sleep(r *http.Request, ms int) {
	select {
	case <-time.After(time.Duration(ms) * time.Millisecond):
	case <-r.Context().Done():
	}
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func solidPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		copy(img.Pix[i:], []byte{magenta.R, magenta.G, magenta.B, 255})
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}
