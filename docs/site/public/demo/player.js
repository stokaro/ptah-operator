/* The recorded-run player.
 *
 * Ported from ptah.run, where the same player replays the same kind of file.
 * What it reads here is demo/recordings/runs.json, written by demo/cmd/record
 * against a live cluster; the page below the frame carries the same transcript
 * as markup, which is what a reader without this script gets. Nothing is
 * invented and nothing is executed: the player replays bytes.
 *
 * Event kinds: `cmd` and `cont` are typed a character at a time, `note` is the
 * demonstration's own narration (typed too), and everything else arrives whole.
 * `sync` moves the pill in the terminal bar, `wait` is a beat, `blank` is a
 * spacer.
 *
 * The comments below name ptah.run's two surfaces -- the hero that plays by
 * itself and the listed sessions that wait to be asked -- because that is where
 * each behavior was decided. This site has only the second: every frame here
 * carries `data-demo-static`, so nothing starts without being asked, and the
 * hero paths are the ones a reader of this copy will not reach.
 *
 * One page, one session. A run has a page of its own and the frame on it plays
 * where it stands, so this copy carries no scenario picker and no flight from
 * one surface to another: what ptah.run does by moving one frame around the
 * page, this site does by linking to the page the session is on. The frame does
 * take the whole window when a reader asks it to, and while it has the window
 * it is the same frame, with the same run in it, parked on the body.
 */
import { CLASS, esc, wideRuns } from "./transcript.mjs";

(function () {
  "use strict";

  var doc = document;

  function $(sel, ctx) {
    return (ctx || doc).querySelector(sel);
  }
  function $$(sel, ctx) {
    return Array.prototype.slice.call((ctx || doc).querySelectorAll(sel));
  }

  var RUNS = window.PTAH_RUNS;
  // A page may list many sessions; only one of them plays. Two typewriters in
  // one column is two things to read, and neither gets read.
  var stopOthers = null;
  // And one of them at most has the window. The frame that takes it covers the
  // page, so a second one would cover the first and leave the reader with two
  // Escapes to press to get back.
  var giveBackWindow = null;

  if (RUNS) $$("[data-demo]").forEach(setupDemo);

  function setupDemo(demo) {
    var screen = $("[data-demo-screen]", demo);
    var transcript = $("[data-demo-transcript]", demo);
    var syncPill = $("[data-demo-sync]", demo);
    var controls = $("[data-demo-controls]", demo);
    var toggleBtn = $("[data-demo-toggle]", demo);
    var speedBtn = $("[data-demo-speed]", demo);
    var speedLabel = $("[data-demo-speed-label]", demo);
    var replayBtn = $("[data-demo-replay]", demo);
    var fullBtn = $("[data-demo-full]", demo);
    var progress = $("[data-demo-progress]", demo);
    var progressFill = $("span", progress);

    var SCENARIOS = RUNS.scenarios;

    // The session this node starts on: what the markup names, or the first
    // pinned one, which is what the home page's transcript carries.
    var first = demo.getAttribute("data-demo-scenario") || RUNS.pinned[0];
    var SCRIPT = SCENARIOS[first].script;


    // What each event is expected to cost. The typing jitter makes the real
    // figure vary by a few per cent, which is invisible on a two-pixel rule and
    // much cheaper than measuring a duration nobody knows before it runs.
    // Every beat is proportional to what the reader has just been given. A
    // one-line answer needs a moment; the lint diagnostic is a paragraph and
    // needs several. Fixed beats were the same length after both, which is why
    // the long one read as too fast and the short one as a stall.
    var TYPE_SCALE = 1.2;

    function typeTime(text) {
      return 460 + text.length * 32 * TYPE_SCALE;
    }

    // Prose is read; a command is scanned for its flags. Both scale, at
    // different rates, and both have a floor so a short line still lands.
    function readTime(text, kind) {
      if (kind === "note") return 900 + text.length * 30;
      return 700 + text.length * 14;
    }

    // A comment and the command under it are one thought, so the reader is not
    // held at the end of the comment while the command it announces waits to be
    // typed -- a page that opens on a comment then holds you there before
    // anything has happened. Half of what that beat was worth moves to the end
    // of the block instead, where the next step is announced.
    var NOTE_BEAT = 260;

    function noteCarry(text) {
      return Math.round(readTime(text, "note") / 2);
    }

    // The beat after output, before the next step is announced. Capped, because
    // past a few seconds a pause stops reading as deliberate.
    function outputBeat(printed) {
      return Math.min(4600, 1000 + printed * 11);
    }

    function isOutput(kind) {
      return kind !== "wait" && kind !== "sync" && kind !== "blank" &&
        kind !== "note" && kind !== "cmd" && kind !== "cont";
    }

    // Whether the event at index i is a command line the next line continues.
    function continues(i) {
      var next = SCRIPT[i];
      return !!next && next[0] === "cont";
    }

    // The duration of every event, walked in order so the beats that depend on
    // preceding output are computed exactly as the run will compute them. The
    // progress rule reads this, so it cannot drift from the keyboard.
    function plan(script) {
      var out = [];
      var printed = 0;
      var carried = 0;
      for (var i = 0; i < script.length; i++) {
        var event = script[i];
        var kind = event[0];
        var ms;
        if (kind === "wait") {
          ms = event[1];
        } else if (kind === "note") {
          ms = outputBeat(printed) + carried + 60 + typeTime(event[1]) + NOTE_BEAT;
          carried = noteCarry(event[1]);
          printed = 0;
        } else if (kind === "cmd" || kind === "cont") {
          ms = typeTime(event[1]);
          if (!(script[i + 1] && script[i + 1][0] === "cont")) {
            ms += readTime(event[1], "cmd");
            printed = 0;
          }
        } else if (kind === "blank") {
          ms = 60;
        } else if (kind === "sync") {
          ms = 120;
        } else {
          ms = 70;
          printed += event[1].length;
        }
        out.push(ms);
      }
      return out;
    }

    var elapsed = 0;
    var duration = 0;
    var timings = [];
    var printed = 0;
    // What the last comment's read beat handed forward, spent at the next one.
    var owed = 0;

    // Set where the rule is going and how long it has to get there, so the
    // browser animates between events and the script never has to tick.
    function advance(ms) {
      var to = duration ? Math.min(1, (elapsed + ms) / duration) : 0;
      progressFill.style.transition =
        ms && !still.matches ? "width " + ms / rate + "ms linear" : "none";
      progressFill.style.width = to * 100 + "%";
      elapsed += ms;
    }

    function freezeProgress() {
      var at = progressFill.getBoundingClientRect().width;
      var of = progress.getBoundingClientRect().width || 1;
      progressFill.style.transition = "none";
      progressFill.style.width = (at / of) * 100 + "%";
      elapsed = duration * (at / of);
    }
    var at = 0;
    var timer = null;
    var blink = null;
    var paused = false;
    var playing = false;
    var seen = false;


    // The screen is rebuilt from a list of finished lines plus the one being
    // typed, so a replay is a reset of that list rather than a DOM rewind.
    var CURSOR = '<span class="demo-cursor" data-on="1">\u258d</span>';
    var lines = [];
    var typing = null;
    var idle = false;

    // Each line is its own block, so a line too long for the frame wraps under
    // its own indent instead of restarting at column zero. A Go annotation or
    // a lint diagnostic broken that way reads as a new top-level line, which
    // is the one thing the shape of a transcript is supposed to tell you.
    function row(inner, wide) {
      // A blank separator is still a row, and an empty block is neither a line
      // on screen nor a line in what a reader selects out of the frame.
      return (
        '<span class="' + (wide ? "l l-wide" : "l") + '">' + (inner || " ") + "</span>"
      );
    }

    function paint() {
      var rows = lines.slice();
      if (typing !== null) {
        var cls = CLASS[typing.kind];
        var head = typing.kind === "cmd" ? '<span class="p">$</span> ' : "";
        var body = esc(typing.text) + CURSOR;
        rows.push(head + (cls ? '<span class="' + cls + '">' + body + "</span>" : body));
      } else if (idle || paused) {
        // A shell that is not being typed at still shows a caret, and the
        // blink is how a reader tells waiting from finished.
        //
        // At the end of the last line, not on a new one: the pause belongs
        // BEFORE the line break. You finish a line, the caret sits where you
        // stopped while you read it, and only then does the session move on.
        // Breaking first and waiting after puts the beat in the wrong place --
        // the reader is looking at an empty row while the thing they were
        // meant to read has already scrolled up a line.
        //
        // `idle` is set only for the beats that are waits. Setting it for the
        // gaps between output lines would add and remove a caret every 70ms.
        if (rows.length) rows[rows.length - 1] += CURSOR;
        else rows.push(CURSOR);
      }
      // Whether to follow is decided before the write, because writing is what
      // moves the bottom. A reader who scrolled up to re-read a finding is not
      // dragged back down by the next line; returning to the bottom re-arms it.
      var following = screen.scrollHeight - screen.scrollTop - screen.clientHeight < 24;
      var wide = wideRuns(rows);
      screen.innerHTML = rows
        .map(function (inner, i) {
          return row(inner, wide[i]);
        })
        .join("");
      if (following) screen.scrollTop = screen.scrollHeight;
    }

    function commit(kind, text) {
      var cls = CLASS[kind];
      var body = esc(text);
      if (kind === "cmd") body = '<span class="p">$</span> ' + body;
      lines.push(cls ? '<span class="' + cls + '">' + body + "</span>" : body);
      // A shell scrolls; the frame is fixed, so the oldest lines go rather
      // than the newest, which is the half a reader is looking at.
      while (lines.length > 200) lines.shift();
      typing = null;
      paint();
    }

    function setSync(state) {
      syncPill.textContent = state;
      syncPill.setAttribute("data-state", state === "no drift" ? "clear" : "pending");
    }

    // Everything above is planned time: the script, the plan, the progress
    // rule. The rate turns it into real time here and nowhere else, so a
    // reader changing the speed cannot make the bar and the keyboard disagree
    // about where in the session they are.
    var RATES = [0.5, 1, 2, 3];
    var rate = 1;
    var pending = null;

    // One chain of timeouts, always. Stopping it drops the beat in flight with
    // it, so nothing can reschedule a beat behind whatever runs next.
    function halt() {
      clearTimeout(timer);
      pending = null;
    }

    // What the beat in flight still owes, in planned time.
    function remaining(beat) {
      return beat ? Math.max(0, beat.ms - (Date.now() - beat.at) * rate) : 0;
    }

    // Pausing stops the chain and keeps the beat, which halt() would throw
    // away. The beat in flight is usually the rest of a line being typed, and
    // step() has already moved the script index past the event that line
    // belongs to -- so resuming through step() would drop whatever was on
    // screen when the reader pressed Pause and overwrite it with the next
    // event.
    function suspend() {
      clearTimeout(timer);
      if (pending) pending = { ms: remaining(pending), fn: pending.fn, at: Date.now() };
    }

    // Resuming finishes that beat if there was one, and otherwise starts the
    // next event. A reader who paused between events is owed no remainder.
    //
    // It stops the chain itself rather than being called after suspend(), which
    // would recompute the remainder: suspend() measures from the moment it ran,
    // so running it again on the way out subtracts the whole pause from the
    // beat it is holding, and a pause longer than the beat leaves nothing of it.
    function resume() {
      clearTimeout(timer);
      var beat = pending;
      pending = null;
      if (beat) return after(beat.ms, beat.fn);
      step();
    }

    function after(ms, fn) {
      halt();
      pending = { ms: ms, fn: fn, at: Date.now() };
      timer = setTimeout(function () {
        var beat = pending;
        pending = null;
        beat.fn();
      }, ms / rate);
    }

    function setRate(next) {
      var beat = pending;
      // Without carrying the remainder a reader who speeds up during a
      // four-second pause waits out the old pause first, and the control reads
      // as broken.
      var left = remaining(beat);
      rate = next;
      speedLabel.textContent = next + "\u00d7";
      speedBtn.setAttribute("aria-label", "Playback speed, " + next + "\u00d7. Press to change.");
      if (beat && playing && !paused) after(left, beat.fn);
    }

    // A wait with a caret through it. The flag is cleared before the next thing
    // runs so that whatever paints next paints without it.
    function hold(ms, fn) {
      idle = true;
      printed = 0;
      paint();
      after(ms, function () {
        idle = false;
        fn();
      });
    }

    function type(kind, text, n) {
      if (paused) return;
      if (n > text.length) {
        return after(kind === "note" ? 240 : 320, function () {
          commit(kind, text);
          // A finished comment gets a breath, not a wait: it introduces the
          // command about to be typed, and half of what it is worth to read is
          // owed to the beat at the end of the block instead.
          if (kind === "note") {
            owed = noteCarry(text);
            return hold(NOTE_BEAT, step);
          }
          // A command that continues on the next line has not been entered
          // yet, so the beat belongs after its last line rather than inside
          // it.
          if (continues(at)) return after(140, step);
          hold(readTime(text, "cmd"), step);
        });
      }
      // The blank row that separates blocks belongs to the note, and it
      // arrives when the note does. Emitting it earlier would end the previous
      // beat on an empty line, which is exactly the line break the beat is
      // supposed to come before.
      if (n === 0 && kind === "note" && lines.length && lines[lines.length - 1] !== "") {
        lines.push("");
      }
      typing = { text: text.slice(0, n), kind: kind };
      paint();
      var ch = text.charAt(n - 1);
      // Prose is read as it lands, so it runs a little quicker than a command,
      // which is scanned character by character for a flag.
      var base = kind === "note" ? 14 : 20;
      var delay = ch === " " ? base + 14 : base + Math.random() * 30;
      after(delay * TYPE_SCALE, function () {
        type(kind, text, n + 1);
      });
    }

    function step() {
      if (paused) return;
      var event = SCRIPT[at];
      if (!event) {
        // A reader who pressed Play asked for one run of it. Nothing is
        // waiting for the reader once the session is over, so the
        // caret goes. Left blinking on a finished frame it asks for input that
        // does not exist, which is the one thing a caret should never say.
        idle = false;
        paint();
        if (!loops()) {
          elapsed = duration;
          advance(0);
          playing = false;
          label();
          return;
        }
        return after(4200, start);
      }
      at++;
      var kind = event[0];
      advance(timings[at - 1] || 0);
      if (kind === "wait") return hold(event[1], step);
      // A note introduces the next step, so the beat before it is the beat
      // after the last one finished: time to read what just happened before
      // being told what happens next.
      if (kind === "note") {
        // The beat before a step is announced: time to read what just happened,
        // plus what the last comment's own beat was owed.
        var settleFor = outputBeat(printed) + owed;
        owed = 0;
        return hold(settleFor, function () {
          type(kind, event[1], 0);
        });
      }
      if (isOutput(kind)) printed += event[1].length;
      if (kind === "sync") {
        setSync(event[1]);
        return after(120, step);
      }
      if (kind === "cmd" || kind === "cont") return type(kind, event[1], 0);
      if (kind === "blank") {
        lines.push("");
        paint();
        return after(60, step);
      }
      commit(kind, event[1]);
      after(70, step);
    }

    // A finished session, not a slower one. Where movement is unwanted the
    // answer is the result, not a longer wait for it.
    function settle() {
      halt();
      demo.classList.remove("is-playing");
      lines = [];
      typing = null;
      for (var i = 0; i < SCRIPT.length; i++) {
        var event = SCRIPT[i];
        if (event[0] === "wait") continue;
        // The same separator the typed run puts in front of a note, so the
        // settled session and the played one are the same text.
        if (event[0] === "note" && lines.length && lines[lines.length - 1] !== "") {
          lines.push("");
        }
        if (event[0] === "sync") setSync(event[1]);
        else if (event[0] === "blank") lines.push("");
        else commit(event[0], event[1]);
      }
      paint();
      playing = false;
      elapsed = duration = 1;
      advance(0);
      label();
    }

    function start() {
      // Whatever else was playing on this page gives way, and this becomes the
      // one thing moving.
      if (stopOthers && stopOthers !== quiet) stopOthers();
      stopOthers = quiet;
      halt();
      at = 0;
      lines = [];
      typing = null;
      idle = false;
      playing = true;
      paused = false;
      elapsed = 0;
      printed = 0;
      owed = 0;
      timings = plan(SCRIPT);
      duration = timings.reduce(function (a, b) { return a + b; }, 0);
      advance(0);
      demo.classList.add("is-playing");
      label();
      paint();
      step();
    }

    // Stop without resetting: another session took over, and this one keeps
    // the frame it had reached so a reader can still read it.
    function quiet() {
      if (!playing) return;
      halt();
      paused = true;
      freezeProgress();
      paint();
      label();
    }

    function label() {
      var next = !playing || paused ? "Play" : "Pause";
      // The icon follows a data attribute rather than being swapped here, so
      // the two shapes live in the markup beside each other and CSS decides
      // which one shows -- the same shape the menu button already uses.
      demo.setAttribute("data-paused", next === "Play" ? "1" : "0");
      toggleBtn.setAttribute("aria-label", next + " the demo");
      toggleBtn.setAttribute("title", next);
      // While nothing is playing, Replay would do what Play does. One button
      // for one action keeps the bar to a single row on a phone.
      replayBtn.hidden = !playing;
    }

    // Two reasons to hold still, one behavior. Reduced motion is a stated
    // preference. A narrow screen is a judgement: the commands wrap there, so
    // typing reflows the block line by line, it costs a phone battery for a
    // 30-second story nobody scrolled down to wait for, and the transcript is
    // the thing a reader can actually take away and paste.
    var still = window.matchMedia("(prefers-reduced-motion: reduce)");
    var narrow = window.matchMedia("(max-width: 720px)");

    function autoplays() {
      // A listed session never starts itself: a page of them would be a page of
      // things moving at once. Pressing Play is a request, and it is honored
      // under reduced motion too -- what the preference governs is what starts
      // without being asked.
      return demo.hasAttribute("data-demo-autoplay") && !still.matches && !narrow.matches;
    }

    // A session that starts itself is ambient and comes round again, because a
    // reader arriving mid-session should not have to guess what the first half
    // said. One that was asked for runs once and stops on its last frame, which
    // is every session on this site.
    function loops() {
      return autoplays();
    }

    // Going live swaps the transcript for the player. On the home page that
    // happens at once, because the demo is the page's first sentence. On a page
    // that lists every session it waits for a press: the transcript is already
    // the answer, and a reader who wants to read rather than watch should not
    // have to wait for a typewriter to catch up with them.
    var live = false;

    function enliven() {
      if (live) return;
      live = true;
      demo.classList.add("is-live");
      screen.hidden = false;
      syncPill.hidden = false;
      controls.hidden = false;
      progress.hidden = false;
      settle();
      watchVisibility();
      scrollerReachable();
      blink = setInterval(function () {
        var cursor = $(".demo-cursor", screen);
        if (!cursor) return;
        // A caret blinks when it is waiting for you and holds steady when it is
        // busy being typed at. Blinking under the keystrokes made the two
        // states one state, and the reader lost the only signal that says
        // "this beat is deliberate, the next line is coming".
        if (typing !== null && !paused) return cursor.setAttribute("data-on", "1");
        cursor.setAttribute("data-on", cursor.getAttribute("data-on") === "1" ? "0" : "1");
      }, 530);
    }

    /* ---- The frame with the window to itself ----
     *
     * Expanding is a state on the frame and nothing more. The chain of
     * timeouts is not touched, so a run that is typing goes on typing through
     * the expansion and through the collapse, at the point it had reached.
     *
     * The stylesheet carries the reasoning for the overlay over the Fullscreen
     * API, because that is where the measurement belongs.
     */

    // What the reader had focused when the frame took the window, and where in
    // the page they were standing.
    var returnFocusTo = null;
    var returnScrollTo = 0;
    var collapseTimer = null;
    var filling = false;
    // Where the frame lives when it is in the page, so that it goes back
    // exactly there and not merely near there.
    var homeParent = null;
    var homeNext = null;

    // The frame spends the expansion as a child of the body.
    //
    // It has to leave the content pane to be seen: Starlight's .main-pane
    // carries `isolation: isolate`, and the site header is fixed outside that
    // stacking context, so the header painted over the top band of a frame that
    // otherwise filled the window and took the press meant for the terminal
    // bar. No z-index written inside that context can reach past it.
    //
    // The player holds these elements, not their place in the document, so the
    // move costs the run nothing. What a move does reset is the scroll position
    // of a scrolling box, which is read and written back around it.
    function moveTo(parent, before) {
      var wasAt = { screen: screen.scrollTop, transcript: transcript.scrollTop };
      parent.insertBefore(demo, before);
      screen.scrollTop = wasAt.screen;
      transcript.scrollTop = wasAt.transcript;
    }

    // How long the stylesheet says the animation runs. Reading it back is what
    // keeps one answer: a duration written here as well would go on waiting
    // after the reduced-motion preference had already taken the animation out.
    function expandMs() {
      var declared = getComputedStyle(demo).getPropertyValue("--demo-expand-ms").trim();
      var ms = parseFloat(declared) || 0;
      return /[^m]s$/.test(declared) ? ms * 1000 : ms;
    }

    // The word, the title and the pressed state all say what the button will do
    // next, which is the pair Play and Pause already show.
    function labelFull() {
      fullBtn.setAttribute("aria-pressed", filling ? "true" : "false");
      fullBtn.setAttribute("title", filling ? "Collapse" : "Expand");
      fullBtn.setAttribute(
        "aria-label",
        filling ? "Collapse the frame back into the page" : "Expand the frame to fill the window"
      );
    }

    // With the window to itself the session is what scrolls, and a scrolling
    // region only a mouse can reach leaves out half the readers. The screen
    // gets no tab stop of its own: it carries aria-hidden, and the player keeps
    // it at the bottom without being asked.
    function scrollerReachable() {
      if (filling && !live) transcript.setAttribute("tabindex", "0");
      else transcript.removeAttribute("tabindex");
    }

    // What a reader can reach with the keyboard while the frame has the window:
    // the bar's own buttons, and the transcript when it is the scrolling region.
    // A hidden button is out, which is what keeps Replay out of the ring while
    // nothing is playing.
    function reachable() {
      return $$("button, [tabindex='0']", demo).filter(function (node) {
        return !node.hidden && node.offsetParent !== null;
      });
    }

    // Tab stays inside the frame while the frame is the window.
    //
    // aria-modal tells a screen reader that the page behind does not exist, and
    // the overlay covers it, so focus reaching the header links behind would
    // put a reader on controls they cannot see and a screen reader on controls
    // it has been told are not there. The ring wraps at both ends rather than
    // stopping, so Shift+Tab off the first control lands on the last.
    function onKey(event) {
      if (event.key === "Escape") {
        event.preventDefault();
        return collapse();
      }
      if (event.key !== "Tab") return;
      var ring = reachable();
      if (ring.length === 0) return;
      var edge = event.shiftKey ? ring[0] : ring[ring.length - 1];
      // Focus outside the frame is the case a wrap cannot handle by itself: the
      // press that put it there came from somewhere this handler never saw.
      if (event.target !== edge && demo.contains(event.target)) return;
      event.preventDefault();
      ring[event.shiftKey ? ring.length - 1 : 0].focus();
    }

    function expand() {
      if (filling) return;
      if (giveBackWindow && giveBackWindow !== collapse) giveBackWindow();
      giveBackWindow = collapse;
      clearTimeout(collapseTimer);
      filling = true;
      returnFocusTo = doc.activeElement;
      returnScrollTo = window.scrollY;
      demo.classList.remove("is-collapsing");
      demo.classList.add("is-expanded");
      homeParent = demo.parentNode;
      homeNext = demo.nextSibling;
      moveTo(doc.body, null);
      // The page behind stops scrolling: the frame covers it, and a wheel over
      // the terminal bar would move a page nobody can see. The frame leaves the
      // flow with it, so the document loses the frame's height; the reader's
      // place in it is given back on the way out.
      doc.documentElement.style.overflow = "hidden";
      demo.setAttribute("role", "dialog");
      demo.setAttribute("aria-modal", "true");
      demo.focus();
      doc.addEventListener("keydown", onKey);
      scrollerReachable();
      labelFull();
    }

    function collapse() {
      if (!filling) return;
      filling = false;
      doc.removeEventListener("keydown", onKey);
      demo.removeAttribute("role");
      demo.removeAttribute("aria-modal");
      demo.classList.add("is-collapsing");
      labelFull();
      scrollerReachable();
      collapseTimer = setTimeout(function () {
        // Back into the page before it rejoins the flow: a frame that stopped
        // being fixed while it was still on the body would stand at the foot of
        // the document for as long as it took to move.
        if (homeParent) moveTo(homeParent, homeNext);
        homeParent = null;
        homeNext = null;
        demo.classList.remove("is-expanded");
        demo.classList.remove("is-collapsing");
        doc.documentElement.style.overflow = "";
        // The frame is back in the flow, so the document is its own height
        // again and the reader's place in it can be given back. Focus goes back
        // after it and without a scroll of its own, which would otherwise take
        // the page to whatever had focus before the frame took the window.
        window.scrollTo(0, returnScrollTo);
        var back =
          returnFocusTo && returnFocusTo.isConnected && returnFocusTo !== doc.body
            ? returnFocusTo
            : fullBtn;
        returnFocusTo = null;
        back.focus({ preventScroll: true });
        if (giveBackWindow === collapse) giveBackWindow = null;
      }, expandMs());
    }

    fullBtn.addEventListener("click", function () {
      if (filling) collapse();
      else expand();
    });
    speedBtn.addEventListener("click", function () {
      setRate(RATES[(RATES.indexOf(rate) + 1) % RATES.length]);
    });
    replayBtn.addEventListener("click", function () {
      enliven();
      start();
    });
    toggleBtn.addEventListener("click", function () {
      enliven();
      if (!playing) return start();
      paused = !paused;
      if (paused) {
        suspend();
        freezeProgress();
      }
      paint();
      label();
      if (!paused) resume();
    });

    function watchVisibility() {
      if (!autoplays()) return;
      if (!("IntersectionObserver" in window)) {
        start();
      } else {
        new IntersectionObserver(function (entries) {
          var visible = entries[0].isIntersecting;
          if (visible && !seen) {
            seen = true;
            start();
            return;
          }
          // Off screen is not paused: the control still says Pause, and coming
          // back resumes rather than restarting somewhere the reader never saw.
          if (!playing || paused) return;
          if (!visible) {
            suspend();
            return freezeProgress();
          }
          // resume() stops the chain itself, so the beat it holds keeps the
          // time it had when the player went off screen.
          resume();
        }, { threshold: 0.25 }).observe(demo);
      }
    }

    // A listed session waits to be asked; the one in the hero is the page. The
    // bar still offers it, because a transcript with no way to watch it play is
    // a transcript, and the point of the page is that it is both.
    labelFull();
    if (demo.hasAttribute("data-demo-static")) {
      controls.hidden = false;
      label();
    } else {
      enliven();
    }
  }
})();
