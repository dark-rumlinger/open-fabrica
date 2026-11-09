package validators

import (
	"context"
	"testing"
)

// The dangerous shape: a lone `.reveal{opacity:0}` revealed only by
// `.reveal.is-in`, with JS adding `is-in` through an IntersectionObserver and the
// class used on real content. The reduced-motion reset is NOT a no-JS fallback, so
// this must flag exactly `.reveal`.
func TestOpacityReveal_FlagsScrollRevealedContent(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "main.css",
		`.reveal{opacity:0; transform:translateY(20px); transition:opacity .8s}
		 .reveal.is-in{opacity:1; transform:none}
		 @media (prefers-reduced-motion:reduce){ .reveal{opacity:1; transform:none} }`)
	saveKind(t, db, "page", "/",
		`<h1 class="reveal">I make quiet, precise things</h1>
		 <p class="reveal">Fifteen years of identity work.</p>`)
	saveKind(t, db, "file", "motion.js",
		`export function initReveals(){
		   const io = new IntersectionObserver(es => es.forEach(e => {
		     if (e.isIntersecting){ e.target.classList.add('is-in'); io.unobserve(e.target); }
		   }));
		   document.querySelectorAll('.reveal').forEach(el => io.observe(el));
		 }`)

	issues := opacityRevealValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 opacity_reveal issue, got %d: %+v", len(issues), issues)
	}
	is := issues[0]
	if is.Category != "opacity_reveal" || is.Signature != "reveal" || is.File != "main.css" {
		t.Errorf("unexpected issue: %+v", is)
	}
	if is.Severity != "error" || !is.Deterministic {
		t.Errorf("opacity_reveal must be a deterministic error: %+v", is)
	}
}

// The safe shape from the guide: content is visible by default (`.fade-up{opacity:1}`)
// and the deferred module arms the entrance by adding a `prep` class, so the
// opacity:0 lives on a COMPOUND selector (never a lone class) AND a visible base
// reset exists. Must NOT flag even though an IntersectionObserver drives the entrance.
func TestOpacityReveal_CleanForVisibleBaseReveal(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "site.css",
		`.fade-up{opacity:1; transform:none}
		 .fade-up.prep{opacity:0; transform:translateY(20px); transition:opacity .6s}
		 .fade-up.prep.in{opacity:1; transform:none}`)
	saveKind(t, db, "page", "/", `<h1 class="fade-up">Visible by default</h1>`)
	saveKind(t, db, "file", "app.js",
		`const io = new IntersectionObserver(es => es.forEach(e => {
		   if (e.isIntersecting) e.target.classList.add('in');
		 }));
		 document.querySelectorAll('.fade-up').forEach(el => { el.classList.add('prep'); io.observe(el); });`)

	if issues := (opacityRevealValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("visible-base reveal must not flag, got: %+v", issues)
	}
}

// No IntersectionObserver anywhere: whatever hides the class, there is no
// scroll/observe reveal to strand content, so the gate stays silent.
func TestOpacityReveal_CleanWhenNoObserver(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "main.css",
		`.reveal{opacity:0} .reveal.is-in{opacity:1}`)
	saveKind(t, db, "page", "/", `<h1 class="reveal">Title</h1>`)
	saveKind(t, db, "file", "app.js",
		`document.querySelector('#btn').addEventListener('click', () =>
		   document.querySelector('.reveal').classList.add('is-in'));`)

	if issues := (opacityRevealValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("no IntersectionObserver must not flag, got: %+v", issues)
	}
}

// A hidden class that no element uses is dead CSS, not a stranded page. Must NOT
// flag even with an observer present for some other class.
func TestOpacityReveal_CleanForUnusedClass(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "main.css",
		`.ghost{opacity:0} .ghost.is-in{opacity:1}`)
	saveKind(t, db, "page", "/", `<h1>No reveal classes here</h1>`)
	saveKind(t, db, "file", "app.js",
		`const io = new IntersectionObserver(()=>{});
		 document.querySelectorAll('.card').forEach(el => io.observe(el));
		 el.classList.add('is-in');`)

	if issues := (opacityRevealValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("unused hidden class must not flag, got: %+v", issues)
	}
}

// A custom cursor hidden until the pointer moves is not a scroll reveal: its state class
// is added by a mousemove handler, even though an IntersectionObserver exists elsewhere.
func TestOpacityReveal_CleanForPointerDrivenState(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "site.css",
		`.cursor-dot{opacity:0; position:fixed}
		 .cursor-dot.is-on{opacity:.9}
		 .fade-up{opacity:1}`)
	saveKind(t, db, "page", "/", `<div class="cursor-dot"></div><h1 class="fade-up">Hi</h1>`)
	saveKind(t, db, "file", "shell.js",
		`const io = new IntersectionObserver(es => es.forEach(e => { if (e.isIntersecting) io.unobserve(e.target); }));
		 document.querySelectorAll('.fade-up').forEach(el => io.observe(el));
		 // padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding padding 
		 const dot = document.querySelector('.cursor-dot');
		 window.addEventListener('mousemove', ev => { dot.classList.add('is-on'); });`)

	if issues := (opacityRevealValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("a pointer-driven state class must not flag, got: %+v", issues)
	}
}
