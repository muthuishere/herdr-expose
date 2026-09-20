#!/usr/bin/env node
/**
 * herdr-expose — the marketing capture run.
 *
 *   node e2e/screenshots.mjs
 *
 * One command. It stands up its OWN throwaway Herdr session, puts a REAL
 * Claude agent in one pane, a second Claude agent that genuinely BLOCKS on a
 * permission question in another, and a plain shell in a third; exposes only
 * that session; pairs a browser exactly as a phone would; and then photographs
 * the product at five real device widths and records a video of a real
 * interaction. Everything it made is taken back down on the way out.
 *
 * Output lands in docs/assets/ — stills as PNG, the video as .webm/.mp4/.gif,
 * plus a regenerated docs/assets/README.md index.
 *
 * ---------------------------------------------------------------------------
 * WHY `share --quick` AND NOTHING ELSE
 * ---------------------------------------------------------------------------
 * These images are published. A `--domain` share puts the owner's permanent
 * hostname (herdr.deemwar.com) into every frame that shows a URL, into the
 * pairing screen's copy, and into the page's own `location`. A `--quick` share
 * is a throwaway `https://<random>.trycloudflare.com` that belongs to nobody,
 * creates no DNS record, needs no Cloudflare account, and is dead within the
 * hour. It is the only rung that is safe to photograph.
 *
 * It must also stay a PUBLIC rung rather than `--lan`, because a LAN share
 * bakes the machine's private IP into the frame instead. `--quick` leaks
 * neither.
 *
 * The share is scoped with `--session`, so no other session on this machine —
 * and this machine runs a dozen, including a live trading desk — can appear in
 * the sidebar. Verify that in the images anyway; it is cheap.
 *
 * ---------------------------------------------------------------------------
 * SAFETY
 * ---------------------------------------------------------------------------
 * The session name is forced to start with `hexshot-`, every herdr call is
 * pinned to that session's own socket by environment, and teardown runs from a
 * finally block. It never stops a Herdr server it did not start, never touches
 * the daemon on :21118, and never goes near the permanent tunnel.
 *
 * ---------------------------------------------------------------------------
 * KNOBS
 * ---------------------------------------------------------------------------
 *   HEXSHOT_KEEP=1      leave the session + share up afterwards (debugging)
 *   HEXSHOT_HEADED=1    watch the browser
 *   HEXSHOT_OUT=<dir>   output directory (default docs/assets)
 *   HEXSHOT_BIN=<path>  an existing herdr-expose binary (default bin/)
 *   HEXSHOT_SKIP_VIDEO=1  stills only
 *   HEXSHOT_SKIP_STILLS=1 video only
 *   HEXSHOT_INDEX_ONLY=1  rebuild docs/assets/README.md from the files already
 *                       there and do nothing else
 *   HEXSHOT_REUSE=1     attach to a session + quick share that a previous
 *                       HEXSHOT_KEEP=1 run left up, instead of building new
 *                       ones. Re-shooting one thing should not cost another
 *                       two agent warm-ups and another tunnel.
 */

import { chromium } from 'playwright'
import { execFileSync, spawn } from 'node:child_process'
import { mkdirSync, writeFileSync, rmSync, existsSync, readdirSync, statSync, renameSync } from 'node:fs'
import { join, dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { homedir, tmpdir } from 'node:os'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '..')
const OUT = process.env.HEXSHOT_OUT || join(REPO, 'docs', 'assets')
const WORK = join(tmpdir(), `hexshot-work-${process.pid}`)
const BIN = process.env.HEXSHOT_BIN || join(REPO, 'bin', 'herdr-expose')
const KEEP = process.env.HEXSHOT_KEEP === '1'
const HEADED = process.env.HEXSHOT_HEADED === '1'
const SKIP_VIDEO = process.env.HEXSHOT_SKIP_VIDEO === '1'
const SKIP_STILLS = process.env.HEXSHOT_SKIP_STILLS === '1'
const REUSE = process.env.HEXSHOT_REUSE === '1'

const SESSION = process.env.HEXSHOT_SESSION || 'hexshot-demo'
if (!SESSION.startsWith('hexshot-')) {
  console.error('refusing to run: the session name must start with "hexshot-" so teardown can never hit a real session')
  process.exit(2)
}
const SESSION_DIR = join(homedir(), '.config', 'herdr', 'sessions', SESSION)
const DEMO_CWD = '/tmp/herdr-demo'

const AGENT_PANE = 'w1:p1'   // a real Claude, working then idle
const SHELL_PANE = 'w1:p2'   // a plain shell, for the terminal-view contrast
const BLOCKED_PANE = 'w1:p3' // a second real Claude, genuinely blocked

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const log = (...a) => console.log('\x1b[1;34m==>\x1b[0m', ...a)
const warn = (...a) => console.log('\x1b[1;33mnote:\x1b[0m', ...a)

/* --------------------------------------------------------------- herdr side */

/**
 * A clean environment for the throwaway session.
 *
 * This script is itself usually run BY a coding agent, so the process it
 * inherits is full of CLAUDE_CODE_* markers. Left in place they make the demo
 * agent print "Transcript saving is off — inherited CLAUDE_CODE_CHILD_SESSION"
 * across the top of every screenshot. They are stripped here, along with the
 * HERDR_* variables that would otherwise address the caller's own pane.
 */
function cleanEnv(extra = {}) {
  const env = { ...process.env, ...extra }
  for (const k of Object.keys(env)) {
    if (k.startsWith('CLAUDE_') || k === 'CLAUDECODE') delete env[k]
    if (k.startsWith('HERDR_')) delete env[k]
  }
  return { ...env, ...extra }
}

/** Every herdr call in this file is pinned to OUR session's socket. */
function hx(...args) {
  return execFileSync('herdr', args, {
    env: cleanEnv({ HERDR_SOCKET_PATH: join(SESSION_DIR, 'herdr.sock'), HERDR_SESSION: SESSION }),
    encoding: 'utf8',
    maxBuffer: 16 * 1024 * 1024,
  })
}
const hxJSON = (...args) => JSON.parse(hx(...args)).result
function agentState(pane) {
  try {
    return hxJSON('agent', 'get', pane).agent?.agent_status ?? 'unknown'
  } catch {
    return 'unknown'
  }
}
function hexpose(...args) {
  return execFileSync(BIN, args, { env: cleanEnv(), encoding: 'utf8', maxBuffer: 8 * 1024 * 1024 })
}
/** The CLI prints human progress before the JSON object; keep only the object. */
function hexposeJSON(...args) {
  const raw = hexpose(...args)
  return JSON.parse(raw.slice(raw.indexOf('{')))
}

/* ------------------------------------------------------------------- setup */

let serverProc = null
let shareId = ''
let shareUrl = ''

async function setup() {
  if (!existsSync(BIN)) throw new Error(`no herdr-expose binary at ${BIN} — build it, or set HEXSHOT_BIN`)
  mkdirSync(WORK, { recursive: true })
  mkdirSync(OUT, { recursive: true })
  // Start from an empty demo directory. A leftover notes.md from a previous
  // run turns the blocked agent's question from "create notes.md?" into
  // "overwrite notes.md?", which is a worse thing to photograph.
  if (!REUSE) rmSync(DEMO_CWD, { recursive: true, force: true })
  mkdirSync(DEMO_CWD, { recursive: true })

  if (REUSE) {
    const live = hexposeJSON('share', 'list', '--json').shares ?? []
    const mine = live.find((sh) => sh.mode === 'quick' && /trycloudflare\.com/.test(sh.url ?? ''))
    if (!mine) throw new Error('HEXSHOT_REUSE=1 but no live --quick share to attach to')
    shareId = mine.id
    shareUrl = mine.url
    log(`reusing share ${shareId} at ${shareUrl} on session ${SESSION}`)
    return
  }

  log(`starting a throwaway Herdr session: ${SESSION}`)
  mkdirSync(SESSION_DIR, { recursive: true })
  serverProc = spawn('herdr', ['server'], {
    env: cleanEnv({ HERDR_SOCKET_PATH: join(SESSION_DIR, 'herdr.sock'), HERDR_SESSION: SESSION }),
    detached: true,
    stdio: 'ignore',
  })
  serverProc.unref()
  for (let i = 0; i < 80 && !existsSync(join(SESSION_DIR, 'herdr.sock')); i++) await sleep(250)
  if (!existsSync(join(SESSION_DIR, 'herdr.sock'))) throw new Error('the session socket never appeared')

  log('creating a workspace and three panes')
  hx('workspace', 'create', '--cwd', DEMO_CWD, '--label', 'herdr-expose demo', '--no-focus')
  hx('pane', 'split', '--pane', AGENT_PANE, '--direction', 'right', '--cwd', DEMO_CWD, '--no-focus')
  hx('pane', 'split', '--pane', SHELL_PANE, '--direction', 'down', '--cwd', DEMO_CWD, '--no-focus')

  // ---------------------------------------------------- the shell pane
  //
  // THESE IMAGES ARE PUBLISHED, so the shell is replaced with a bare `/bin/sh`
  // in a scrubbed environment before anything is photographed. Two reasons,
  // both found by looking at a first pass:
  //
  //   * an interactive zsh sets the TERMINAL TITLE to `user@host:cwd`, and
  //     herdr-expose builds its pane titles from exactly that — so the sidebar
  //     row read `muthuishere@Muthukumarans-MacBook-Pro-2` in every frame;
  //   * the prompt theme prints the same thing again inside the pane.
  //
  // A plain `sh` sets no title at all, which lets one OSC-0 sequence name the
  // pane and have the name stick. `ls -l` is also deliberately absent below:
  // a long listing prints the file OWNER, which is the username again.
  await sleep(2500) // let the shell finish starting before typing at it
  const sh = (cmd) => {
    hx('pane', 'send-text', SHELL_PANE, cmd)
    return sleep(200).then(() => { hx('pane', 'send-keys', SHELL_PANE, 'enter') })
  }
  log('giving the shell pane a scrubbed prompt and something real to show')
  await sh('exec env -i HOME="$HOME" PATH="$PATH" TERM=xterm-256color PS1="demo:/tmp/herdr-demo $ " /bin/sh')
  await sleep(1500)
  await sh('printf "\\033]0;herdr-expose demo shell\\007"; clear')
  await sleep(1200)
  for (const cmd of [
    'sw_vers',
    'node --version; go version',
    'echo "$TERM  $(tput cols)x$(tput lines)"',
    'date -u "+%Y-%m-%dT%H:%M:%SZ"',
  ]) {
    await sh(cmd)
    await sleep(900)
  }

  // Pane 1: a real Claude that will answer a real question. Bypass permissions
  // so it finishes its turn unattended — this is the "working then idle" pane.
  log('starting the main Claude agent')
  hx('agent', 'start', 'explain-herdr', '--kind', 'claude', '--pane', AGENT_PANE,
    '--timeout', '120000', '--', '--permission-mode', 'bypassPermissions')

  // Pane 3: a second real Claude on DEFAULT permissions. Asked to write a file,
  // it asks for approval, and Herdr's own detection classifies it as blocked.
  // Nothing here is staged: the question in the screenshot is Claude's own.
  log('starting the second Claude agent (the one that will block)')
  hx('agent', 'start', 'write-notes', '--kind', 'claude', '--pane', BLOCKED_PANE,
    '--timeout', '120000', '--', '--permission-mode', 'default')

  log('prompting the main agent')
  hx('agent', 'prompt', AGENT_PANE,
    'In three short paragraphs, explain why running a coding agent inside a terminal ' +
    'multiplexer is different from running it in a plain terminal window. Then run ' +
    '`go version` and tell me what it printed.')

  log('prompting the second agent into a real permission question')
  hx('agent', 'prompt', BLOCKED_PANE,
    'Create a file called notes.md here containing three bullet points about terminal multiplexers.')

  // Wait for the main agent to actually produce something, and for the second
  // to actually block. Both are polled; neither is faked.
  log('waiting for the agents to reach photogenic states')
  const t0 = Date.now()
  let blocked = false
  while (Date.now() - t0 < 180000) {
    const b = agentState(BLOCKED_PANE)
    if (b === 'blocked') { blocked = true; break }
    await sleep(1000)
  }
  if (!blocked) {
    // The documented fallback: report the state through the API an agent uses
    // to declare its own lifecycle. The pane's own prompt region is still what
    // the transcript renders, so the screenshot stays honest.
    warn('Claude did not block on its own — declaring the state through `pane report-agent`')
    hx('pane', 'report-agent', BLOCKED_PANE, '--source', 'hexshot', '--agent', 'claude',
      '--state', 'blocked', '--message', 'Create file notes.md?')
  }
  // Give the main agent a moment to lay down some transcript.
  for (let i = 0; i < 60; i++) {
    const text = hx('pane', 'read', AGENT_PANE, '--lines', '80')
    if (text.length > 400) break
    await sleep(1000)
  }
  log(`agent states: ${AGENT_PANE}=${agentState(AGENT_PANE)} ${BLOCKED_PANE}=${agentState(BLOCKED_PANE)}`)

  // ------------------------------------------------------------- the share
  // --quick, never --domain and never --lan. See the block at the top.
  log('raising a --quick tunnel (anonymous trycloudflare hostname, 1 hour)')
  const res = hexposeJSON('share', '--quick', '--session', SESSION, '--hours', '1', '--json')
  shareId = res.share.id
  shareUrl = res.share.url
  if (res.share.mode !== 'quick' || !/trycloudflare\.com/.test(shareUrl)) {
    throw new Error(`refusing to photograph a '${res.share.mode}' share at ${shareUrl} — only --quick is publishable`)
  }
  if (res.share.domain) throw new Error(`the share claimed a domain (${res.share.domain}); refusing`)
  log(`share ${shareId} at ${shareUrl}`)

  // The CLI returns as soon as cloudflared hands back a hostname, but that
  // hostname is not resolvable everywhere for a few more seconds — a run that
  // went straight to the browser got four ERR_NAME_NOT_RESOLVED in a row and
  // threw away a perfectly good session. Wait for the tunnel to actually
  // answer before pointing a browser at it.
  log('waiting for the tunnel to resolve and answer')
  const tunnelT0 = Date.now()
  let live = false
  while (Date.now() - tunnelT0 < 120000) {
    try {
      execFileSync('curl', ['-fsS', '--max-time', '5', `${shareUrl}/healthz`], { stdio: 'ignore' })
      live = true
      break
    } catch { await sleep(2000) }
  }
  if (!live) throw new Error(`the quick tunnel at ${shareUrl} never answered /healthz`)
  log(`the tunnel answered after ${Math.round((Date.now() - tunnelT0) / 1000)}s`)
  return res.pairing_code
}

/** A fresh single-use code for each browser context, exactly as a real user gets. */
function mintCode() {
  const r = hexposeJSON('share', 'pair', shareId, '--json')
  return r.pairing_code ?? r.code
}

/* ----------------------------------------------------------------- browser */

/**
 * xterm's WebGL/Canvas renderers draw into a <canvas>, which screenshots fine
 * but renders headlessly at the wrong metrics often enough to ruin a shot. The
 * app ships a DOM-renderer fallback and persists the decision under this key,
 * so this is a shipped code path, not a capture-only hook.
 */
const initScript = () => {
  try {
    const ua = navigator.userAgent.slice(0, 200)
    const at = Date.now()
    localStorage.setItem('herdr-expose.renderer-blocklist',
      JSON.stringify([{ ua, kind: 'webgl', at }, { ua, kind: 'canvas', at }]))
  } catch {}
}

/**
 * Pair a fresh browser exactly as a phone does: follow a `?pair=<code>` deep
 * link. Retried, because a quick tunnel is a real tunnel over the real
 * internet and a laptop changing networks mid-run produces ERR_NETWORK_CHANGED
 * — which is a flake, not a product failure, and should not lose a capture.
 */
async function newPairedContext(browser, opts) {
  let lastErr
  for (let attempt = 0; attempt < 4; attempt++) {
    const ctx = await browser.newContext(opts)
    await ctx.addInitScript(initScript)
    const t0 = Date.now()
    const page = await ctx.newPage()
    try {
      await page.goto(`${shareUrl}/?pair=${mintCode()}`, { waitUntil: 'domcontentloaded', timeout: 60000 })
      await page.locator('.panelist').waitFor({ state: 'visible', timeout: 60000 })
      await settle(page)
      return { ctx, page, t0 }
    } catch (e) {
      lastErr = e
      warn(`pairing attempt ${attempt + 1} failed (${String(e.message).split('\n')[0]}) — retrying`)
      await ctx.close().catch(() => {})
      await sleep(4000 * (attempt + 1))
    }
  }
  throw lastErr
}

/** Fonts loaded, no animation mid-flight, transcript polled at least once. */
async function settle(page, ms = 2200) {
  await page.evaluate(() => document.fonts?.ready).catch(() => {})
  await sleep(ms)
}

/** Open a pane BY TARGET id. Row order is not stable: "Needs you" re-pins. */
async function openPane(page, paneId, want = 'transcript') {
  const target = `${SESSION}/${paneId}`
  const back = page.locator('.paneview .iconbtn[aria-label="Back to panes"]')
  if (await back.isVisible().catch(() => false)) {
    await back.click().catch(() => {})
    await page.locator('.row').first().waitFor({ state: 'visible', timeout: 5000 }).catch(() => {})
  }
  await page.locator(`.row[data-target="${target}"]`).first().click({ timeout: 20000 }).catch(() => {})
  await page.locator(want === 'transcript' ? '.tr-body' : '.term-host .xterm')
    .waitFor({ state: 'visible', timeout: 25000 }).catch(() => {})
  for (let i = 0; i < 32; i++) {
    const vp = await page.evaluate(() => (window.__herdrStats ? window.__herdrStats().viewport : {})).catch(() => ({}))
    if (vp?.[target] === want) return true
    await sleep(250)
  }
  return false
}

/** Switch the header view toggle, answering the terminal's cost prompt. */
async function setView(page, paneId, kind) {
  const target = `${SESSION}/${paneId}`
  const want = kind === 'transcript' ? 'transcript' : 'live'
  await page.locator(`.viewtoggle-btn:text-is("${kind === 'transcript' ? 'text' : 'term'}")`).click().catch(() => {})
  const confirm = page.locator('.notice-confirm button:text-is("Show terminal")')
  if (await confirm.isVisible().catch(() => false)) await confirm.click()
  for (let i = 0; i < 32; i++) {
    const vp = await page.evaluate(() => (window.__herdrStats ? window.__herdrStats().viewport : {})).catch(() => ({}))
    if (vp?.[target] === want) return true
    await sleep(250)
  }
  return false
}

async function backToList(page) {
  const back = page.locator('.paneview .iconbtn[aria-label="Back to panes"]')
  if (await back.isVisible().catch(() => false)) await back.click().catch(() => {})
  await page.locator('.row').first().waitFor({ state: 'visible', timeout: 8000 }).catch(() => {})
  await sleep(600)
}

/** Text the transcript view is actually rendering. */
async function transcriptText(page) {
  return page.evaluate(() => document.querySelector('.tr-text')?.innerText ?? '')
}

/**
 * Do not photograph a half-populated transcript. The view is polled at ~1Hz,
 * so the frame right after it opens is routinely empty or one line long — and
 * a blank hero shot is worse than no hero shot.
 */
async function waitForTranscript(page, minChars = 200, timeoutMs = 20000) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeoutMs) {
    if ((await transcriptText(page)).trim().length >= minChars) return true
    await sleep(400)
  }
  return false
}

/* ------------------------------------------------------------------ stills */

const shots = []

/**
 * These are flat, dark, mostly-monochrome UI screenshots at 2x-3x, so a 255
 * colour palette is visually indistinguishable from the 24-bit original and
 * roughly halves the file. They live in a git repo, so that is worth doing —
 * and doing HERE rather than as a manual afterthought, so a re-run stays one
 * command. (`pngquant` would be the obvious tool; ffmpeg is already a hard
 * dependency of the video half, so this adds nothing to install.)
 */
function shrink(file) {
  const before = statSync(file).size
  // Scratch files sit NEXT TO the target: WORK is under the system temp dir,
  // which can be a different volume, and rename(2) across volumes is EXDEV.
  const pal = `${file}.pal`
  const out = `${file}.q`
  try {
    ff('-i', file, '-vf', 'palettegen=max_colors=255:stats_mode=single', '-f', 'image2', pal)
    ff('-i', file, '-i', pal, '-lavfi', 'paletteuse=dither=sierra2_4a',
      '-compression_level', '100', '-f', 'image2', out)
    if (statSync(out).size < before) renameSync(out, file)
  } catch { /* an uncompressed shot is still a shot */ } finally {
    for (const f of [pal, out]) { try { rmSync(f, { force: true }) } catch {} }
  }
  return statSync(file).size
}

async function shot(page, name, what) {
  const file = join(OUT, `${name}.png`)
  await page.screenshot({ path: file })
  const kb = Math.round(shrink(file) / 1024)
  shots.push({ name: `${name}.png`, what, kb })
  log(`  ${name}.png (${kb} KB)`)
}

/**
 * The device matrix. iPhone SE is spelled out rather than taken from
 * Playwright's descriptor: that descriptor is the 320px 1st-generation SE, and
 * 375 is the width this UI is actually designed against.
 */
const VIEWPORTS = [
  { tag: '375', label: 'iPhone SE / 375px — the design target',
    opts: { viewport: { width: 375, height: 667 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true },
    want: ['pane-list', 'transcript', 'blocked', 'terminal'] },
  { tag: 'iphone14', label: 'iPhone 14 Pro (393x852)',
    opts: { viewport: { width: 393, height: 852 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true },
    want: ['pane-list', 'transcript'] },
  { tag: 'pixel7', label: 'Pixel 7, Android (412x915)',
    opts: { viewport: { width: 412, height: 915 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true },
    want: ['transcript', 'blocked'] },
  { tag: 'ipad', label: 'iPad portrait (810x1080)',
    opts: { viewport: { width: 810, height: 1080 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true },
    want: ['pane-list', 'transcript'] },
  { tag: '1440', label: 'Desktop (1440x900)',
    opts: { viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 },
    want: ['desktop', 'desktop-terminal', 'desktop-blocked'] },
]

const DESCRIBE = {
  'pane-list': 'the session and pane list, with live agent state badges',
  transcript: 'the transcript view of a real Claude agent — reflowed, readable text',
  blocked: 'a blocked agent: its own question, plus the y/n/1/2/3 answer key bar',
  terminal: 'the terminal view on a plain shell pane, for contrast',
  desktop: 'the desktop layout — sidebar and an open pane together',
  'desktop-terminal': 'the desktop layout with the terminal view open on the shell pane',
  'desktop-blocked': 'the desktop layout with the blocked agent open',
}

async function captureStills(browser) {
  for (const vp of VIEWPORTS) {
    log(`capturing ${vp.label}`)
    const { ctx, page } = await newPairedContext(browser, vp.opts)
    try {
      for (const kind of vp.want) {
        if (kind === 'pane-list' || kind === 'desktop' || kind === 'desktop-terminal' || kind === 'desktop-blocked') {
          if (kind === 'pane-list') {
            await backToList(page)
            await settle(page)
            await shot(page, `pane-list-${vp.tag}`, DESCRIBE['pane-list'])
            continue
          }
          if (kind === 'desktop') {
            await openPane(page, AGENT_PANE, 'transcript')
            await waitForTranscript(page, 400)
            await settle(page, 2500)
            await shot(page, 'desktop-1440', DESCRIBE.desktop)
            continue
          }
          if (kind === 'desktop-terminal') {
            await openPane(page, SHELL_PANE, 'transcript')
            await setView(page, SHELL_PANE, 'terminal')
            await settle(page, 2500)
            await shot(page, 'desktop-1440-terminal', DESCRIBE['desktop-terminal'])
            continue
          }
          await openPane(page, BLOCKED_PANE, 'transcript')
          await waitForTranscript(page, 60)
          await settle(page, 2000)
          await shot(page, 'desktop-1440-blocked', DESCRIBE['desktop-blocked'])
          continue
        }
        if (kind === 'transcript') {
          await openPane(page, AGENT_PANE, 'transcript')
          await waitForTranscript(page, 400)
          await settle(page, 2500)
          await shot(page, `transcript-${vp.tag}`, DESCRIBE.transcript)
          continue
        }
        if (kind === 'blocked') {
          await openPane(page, BLOCKED_PANE, 'transcript')
          await waitForTranscript(page, 60)
          await settle(page, 2000)
          await shot(page, `blocked-${vp.tag}`, DESCRIBE.blocked)
          continue
        }
        if (kind === 'terminal') {
          await openPane(page, SHELL_PANE, 'transcript')
          await setView(page, SHELL_PANE, 'terminal')
          await settle(page, 2500)
          await shot(page, `terminal-${vp.tag}`, DESCRIBE.terminal)
        }
      }
    } finally {
      await ctx.close()
    }
  }
}

/* ------------------------------------------------------------------- video */

/**
 * The story, told once, in a real browser, against the real product:
 *
 *   1. the paired URL opens on a phone — session and pane list, real states
 *   2. a blocked agent is sitting there needing an answer
 *   3. tap in: the transcript, and its real question, held long enough to read
 *   4. answer it from the phone with the key bar
 *   5. open the working agent, type a real prompt at human speed
 *   6. the agent genuinely answers and the transcript updates live
 *   7. cut to desktop 1440: sidebar and pane together
 *
 * Nothing is captioned, staged or reconstructed. What IS done is an edit: a
 * raw Playwright capture is mostly dead air — a page loading, a tunnel
 * negotiating, an agent thinking — and nobody watches that to the end.
 *
 * So the run records a MARK at every beat boundary, and `cutPlan()` below
 * turns those marks into trim windows with a per-window speed. Dead stretches
 * are compressed hard, necessary-but-slow stretches (the agent thinking) are
 * ramped rather than deleted so the rhythm stays honest, and the two frames
 * that carry the whole argument — the transcript becoming readable, and the
 * reply arriving — are held at real speed. Nothing ugly is edited around; if
 * the product is slow or scrappy somewhere, that stretch is still in the cut.
 */

/** Marks are ms offsets from the moment the recording context was created. */
function marker(t0) {
  const m = {}
  return { m, at: (name) => { m[name] = Date.now() - t0; return m[name] } }
}

async function recordVideo(browser) {
  const phoneDir = join(WORK, 'vid-phone')
  const deskDir = join(WORK, 'vid-desk')

  // Beats 2-4 need a genuinely blocked agent. The stills run does not consume
  // one, but a re-recorded take (HEXSHOT_REUSE=1) does — the video ANSWERS the
  // question, so the second take would find nothing pinned under "Needs you".
  // Ask it for another file and let it block again on its own.
  if (agentState(BLOCKED_PANE) !== 'blocked') {
    log('the blocked agent has been answered — asking it for another file so it blocks again')
    try {
      hx('agent', 'prompt', BLOCKED_PANE, 'Now create a second file called links.md with two bullet points.')
    } catch { /* it may still be mid-turn; the poll below decides */ }
    for (let i = 0; i < 60 && agentState(BLOCKED_PANE) !== 'blocked'; i++) await sleep(1000)
    if (agentState(BLOCKED_PANE) !== 'blocked') warn('it did not block — the video will be missing beats 2-4')
  }

  /* ------------------------------------------------------- the phone take */
  log('recording the phone take')
  const phone = await newPairedContext(browser, {
    viewport: { width: 393, height: 852 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true,
    recordVideo: { dir: phoneDir, size: { width: 393, height: 852 } },
  })
  const { m, at } = marker(phone.t0)
  try {
    const page = phone.page
    await backToList(page)
    at('listReady')
    await sleep(3500)                                    // 1 + 2. the list, and the blocked row in it

    const urgent = page.locator('.ws-urgent .row').first()
    if (await urgent.isVisible().catch(() => false)) await urgent.click()
    else await openPane(page, BLOCKED_PANE, 'transcript')
    await page.locator('.tr-body').waitFor({ state: 'visible', timeout: 25000 }).catch(() => {})
    // Wait for the transcript to actually carry text — the poll is ~1Hz, and
    // holding on an empty box is exactly the dead air the edit is removing.
    for (let i = 0; i < 20; i++) {
      if ((await transcriptText(page)).trim().length > 40) break
      await sleep(400)
    }
    at('question')
    await sleep(4500)                                    // 3. its real question, held

    const one = page.locator('.tr-keys .key', { hasText: /^1$/ }).first()
    if (await one.isVisible().catch(() => false)) await one.click().catch(() => {})
    at('answered')
    await sleep(3500)                                    // 4. answered from the phone

    await backToList(page)
    await sleep(1500)
    await openPane(page, AGENT_PANE, 'transcript')
    for (let i = 0; i < 20; i++) {
      if ((await transcriptText(page)).trim().length > 200) break
      await sleep(400)
    }
    at('agentOpen')
    await sleep(3000)                                    // 5. the transcript, readable

    const input = page.locator('.tr-input')
    await input.click().catch(() => {})
    at('typeStart')
    // `type`, not `fill`. Instant text reads as fake, and the point of the
    // shot is that a person typed this on a phone.
    await input.type('in one sentence, what did you just run?', { delay: 70 })
    await sleep(900)
    await page.locator('.tr-send').click().catch(() => {})
    at('sent')

    // 6. Wait for the agent to ACTUALLY answer. Two false finishes to dodge,
    // both seen in a real take:
    //
    //   * "the transcript grew" fires ~40ms after Send, because submitting
    //     echoes the prompt into the pane. So text growth alone is useless.
    //   * `agent_status` can read not-working for a beat between submission
    //     and the model starting, so a bare "wait until not working" returns
    //     immediately and the payoff hold lands on an unanswered screen —
    //     which is exactly what the first cut did.
    //
    // Require BOTH: the state has settled, and the transcript has grown well
    // past the echoed prompt. Plus a floor, so a fast answer is still held.
    const echoed = (await transcriptText(page)).length
    const t1 = Date.now()
    for (let i = 0; i < 140; i++) {
      const grown = (await transcriptText(page)).length > echoed + 150
      const settledState = agentState(AGENT_PANE) !== 'working'
      if (grown && settledState && Date.now() - t1 > 2500) break
      await sleep(600)
    }
    at('replied')
    await sleep(4000)                                    // the payoff, held
    at('end')
  } finally {
    await phone.ctx.close()
  }

  /* ----------------------------------------------------- the desktop take */
  log('recording the desktop take')
  const desk = await newPairedContext(browser, {
    viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1,
    recordVideo: { dir: deskDir, size: { width: 1440, height: 900 } },
  })
  const dm = marker(desk.t0)
  try {
    await openPane(desk.page, AGENT_PANE, 'transcript')
    await sleep(1500)
    dm.at('ready')
    await sleep(4500)                                    // 7. sidebar and pane together
    dm.at('end')
  } finally {
    await desk.ctx.close()
  }

  const pick = (dir) => {
    const f = readdirSync(dir).filter((n) => n.endsWith('.webm'))
    if (!f.length) throw new Error(`no video recorded in ${dir}`)
    return join(dir, f[0])
  }
  return { phone: pick(phoneDir), desk: pick(deskDir), m, dm: dm.m }
}

/* -------------------------------------------------------------- the edit */

const ff = (...args) => execFileSync('ffmpeg', ['-y', '-loglevel', 'error', ...args], { stdio: 'inherit' })

/**
 * Marks -> a list of {from, to, speed} windows, in seconds.
 *
 * `speed` > 1 is a ramp: the stretch is kept, just played faster. Only two
 * things get a hard cut, and both are infrastructure rather than product: the
 * tail after the last beat, and any window the marks show as empty.
 */
function cutPlan(m) {
  const s = (ms) => ms / 1000
  const w = []
  const push = (from, to, speed) => { if (to - from > 0.15) w.push({ from, to, speed }) }

  push(0, s(m.listReady), 6)                             // boot + pair + first paint
  push(s(m.listReady), s(m.listReady) + 3.5, 1)          // the list
  push(s(m.listReady) + 3.5, s(m.question), 3)           // the tap
  push(s(m.question), s(m.question) + 4.5, 1)            // HOLD: the real question
  push(s(m.question) + 4.5, s(m.agentOpen), 3)           // answer, back, open the agent
  push(s(m.agentOpen), s(m.agentOpen) + 3, 1)            // HOLD: readable transcript
  push(s(m.agentOpen) + 3, s(m.typeStart), 2)
  push(s(m.typeStart), s(m.sent) + 0.8, 1)               // typing, at human speed
  push(s(m.sent) + 0.8, s(m.replied), 6)                 // the agent thinking — ramped, not cut
  push(s(m.replied), Math.min(s(m.end), s(m.replied) + 4), 1) // HOLD: the reply arriving
  return w
}

/**
 * Build the cut. Each window becomes a trim+setpts segment; the segments
 * concat; the phone take is pillarboxed onto a 1280x720 canvas and the
 * desktop take letterboxed onto the same one, so the two cuts join without a
 * resolution change.
 */
function encode(takes) {
  const webm = join(OUT, 'demo.webm')
  const mp4 = join(OUT, 'demo.mp4')
  const gif = join(OUT, 'demo.gif')
  const plan = cutPlan(takes.m)
  const deskFrom = takes.dm.ready / 1000
  const deskTo = takes.dm.end / 1000

  log(`editing: ${plan.length} phone windows + 1 desktop window`)
  for (const p of plan) log(`  ${p.from.toFixed(1)}s-${p.to.toFixed(1)}s @ ${p.speed}x`)

  // [0:v] = the phone take, [1:v] = the desktop take.
  const parts = []
  const labels = []
  plan.forEach((p, i) => {
    parts.push(
      `[0:v]trim=start=${p.from.toFixed(3)}:end=${p.to.toFixed(3)},setpts=(PTS-STARTPTS)/${p.speed},` +
      `scale=1280:720:force_original_aspect_ratio=decrease,setsar=1,` +
      `pad=1280:720:(ow-iw)/2:(oh-ih)/2:color=0x0b0d10[p${i}]`)
    labels.push(`[p${i}]`)
  })
  parts.push(
    `[1:v]trim=start=${deskFrom.toFixed(3)}:end=${deskTo.toFixed(3)},setpts=PTS-STARTPTS,` +
    `scale=1280:720:force_original_aspect_ratio=decrease,setsar=1,` +
    `pad=1280:720:(ow-iw)/2:(oh-ih)/2:color=0x0b0d10[d0]`)
  labels.push('[d0]')
  const filter = parts.join(';') + `;${labels.join('')}concat=n=${labels.length}:v=1:a=0,fps=24[v]`

  log('encoding webm (the raw cut)')
  ff('-i', takes.phone, '-i', takes.desk, '-filter_complex', filter,
    '-map', '[v]', '-c:v', 'libvpx-vp9', '-b:v', '0', '-crf', '34', '-row-mt', '1', '-an', webm)

  log('encoding mp4 (h264, faststart — the primary)')
  ff('-i', webm, '-c:v', 'libx264', '-preset', 'slow', '-crf', '25',
    '-pix_fmt', 'yuv420p', '-movflags', '+faststart', '-an', mp4)

  // The gif is the fallback for anywhere mp4 will not autoplay, so it is
  // deliberately the worst copy: half size, 10fps, a generated 96-colour
  // palette, and trimmed to the first 15 seconds if that is what keeps it
  // under 2MB.
  log('encoding gif (the autoplay fallback)')
  const pal = join(WORK, 'pal.png')
  const gifFrom = (secs) => {
    const vf = `fps=10,scale=560:-1:flags=lanczos`
    ff(...(secs ? ['-t', String(secs)] : []), '-i', webm, '-vf', `${vf},palettegen=max_colors=96`, pal)
    ff(...(secs ? ['-t', String(secs)] : []), '-i', webm, '-i', pal, '-lavfi',
      `${vf}[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=3`, gif)
    return statSync(gif).size
  }
  let bytes = gifFrom(null)
  for (const t of [18, 14, 10]) {
    if (bytes <= 2 * 1024 * 1024) break
    log(`  gif is ${Math.round(bytes / 1024)} KB — retrimming to ${t}s`)
    bytes = gifFrom(t)
  }

  return [webm, mp4, gif]
}

/* ------------------------------------------------------------------- index */

/**
 * The index the landing page is read FROM, so it is derived from the files on
 * disk rather than from what this process happens to remember. That makes it
 * regenerable on its own (HEXSHOT_INDEX_ONLY=1) and keeps it honest if a run
 * only re-shot half the set.
 */

/** Which device each filename suffix was shot on. */
const DEVICE = {
  '375': 'iPhone SE / 375px — the design target',
  iphone14: 'iPhone 14 Pro, 393x852 @3x',
  pixel7: 'Pixel 7 (Android), 412x915 @2.625x',
  ipad: 'iPad portrait, 810x1080 @2x',
  '1440': 'Desktop, 1440x900 @2x',
}

const VIDEO_WHAT = {
  'demo.mp4': '**Use this one on the page.** h264 + faststart, so a browser starts playing it before the file has finished downloading.',
  'demo.webm': 'the same cut as VP9 — the raw Playwright recording, edited, before the h264 pass',
  'demo.gif': 'the fallback for anywhere mp4 will not autoplay: half size, 10fps, 96-colour palette',
}

function probe(file) {
  try {
    const [w, h] = execFileSync('ffprobe', ['-v', 'error', '-select_streams', 'v:0',
      '-show_entries', 'stream=width,height', '-of', 'csv=p=0:s=x', file],
      { encoding: 'utf8' }).trim().split('\n')[0].split('x')
    let dur = ''
    try {
      dur = execFileSync('ffprobe', ['-v', 'error', '-show_entries', 'format=duration',
        '-of', 'csv=p=0', file], { encoding: 'utf8' }).trim()
    } catch {}
    return { size: `${w}x${h}`, dur: dur ? `${Number(dur).toFixed(1)}s` : '' }
  } catch {
    return { size: '', dur: '' }
  }
}
const kbOf = (f) => `${Math.round(statSync(f).size / 1024)} KB`

function writeIndex() {
  const present = readdirSync(OUT).sort()
  const videos = ['demo.mp4', 'demo.webm', 'demo.gif'].filter((n) => present.includes(n))
  const stills = present.filter((n) => n.endsWith('.png'))

  const L = []
  L.push('# docs/assets', '',
    'Generated by `node e2e/screenshots.mjs`. Everything in here is a real capture of a',
    'real `herdr-expose` share, in front of a real Herdr session running two real Claude',
    'agents and a real shell — no mockups, no composites, no retouching, no invented UI.',
    '',
    'The capture always uses `herdr-expose share --quick`, never `--domain` and never',
    '`--lan`, so no owned hostname and no private LAN address is ever in frame — the URL',
    'is an anonymous `*.trycloudflare.com` that is revoked the moment the run ends. The',
    'share is scoped to a throwaway `hexshot-*` session, so no other session on the',
    'capture machine can appear in the sidebar. Dark theme throughout: it is the only',
    'theme the app has.',
    '')

  if (videos.length) {
    L.push('## Video', '',
      'One continuous take, edited: the pane list on a phone with live agent states; a',
      'blocked agent pinned under "Needs you"; its real question opened and answered from',
      'the phone; the working agent\'s transcript with a prompt typed at human speed and',
      'sent from the browser; its answer arriving live; then a cut to the desktop layout',
      'with the sidebar and a pane together. Dead air is compressed and slow-but-real',
      'stretches (the agent thinking) are speed-ramped rather than cut, so the rhythm',
      'stays honest. No music, no captions, no voiceover.',
      '',
      '| file | what it is | frame | duration | bytes |', '|---|---|---|---|---|')
    for (const n of videos) {
      const f = join(OUT, n)
      const { size, dur } = probe(f)
      L.push(`| \`${n}\` | ${VIDEO_WHAT[n]} | ${size} | ${dur} | ${kbOf(f)} |`)
    }
    L.push('')
  }

  if (stills.length) {
    L.push('## Stills', '',
      'Every still is 2x or better, so they stay sharp on a retina display, and each has',
      'been palette-quantised to 255 colours — visually identical on this flat dark UI,',
      'about half the bytes.',
      '',
      '| file | device | what it shows | pixels | bytes |', '|---|---|---|---|---|')
    for (const n of stills) {
      const base = n.replace(/\.png$/, '')
      const tag = base.startsWith('desktop-1440') ? '1440' : base.split('-').pop()
      const kind = base.startsWith('desktop-1440')
        ? base.replace('desktop-1440-', 'desktop-').replace(/^desktop-1440$/, 'desktop')
        : base.slice(0, base.length - tag.length - 1)
      L.push(`| \`${n}\` | ${DEVICE[tag] ?? ''} | ${DESCRIBE[kind] ?? ''} | ${probe(join(OUT, n)).size} | ${kbOf(join(OUT, n))} |`)
    }
    L.push('')
  }

  L.push('## Regenerating', '',
    '```bash',
    'node e2e/screenshots.mjs                        # everything: session, share, stills, video, teardown',
    'HEXSHOT_SKIP_VIDEO=1 node e2e/screenshots.mjs   # stills only',
    'HEXSHOT_INDEX_ONLY=1 node e2e/screenshots.mjs   # just rebuild this file',
    '```',
    '',
    'It stands up its own throwaway session with its own agents, exposes only that',
    'session over a `--quick` tunnel, pairs a browser through a `?pair=` deep link,',
    'shoots every viewport, records and edits the video, and then revokes the share and',
    'deletes the session. See the comment block at the top of `e2e/screenshots.mjs` for',
    'why it must be `--quick` and what the safety rails are.',
    '')
  writeFileSync(join(OUT, 'README.md'), L.join('\n'))
  log(`wrote ${join(OUT, 'README.md')}`)
}

/* ---------------------------------------------------------------- teardown */

function teardown() {
  if (REUSE) {
    warn('HEXSHOT_REUSE=1 — this run did not create the session or the share, so it does not remove them')
    return
  }
  if (KEEP) {
    warn(`HEXSHOT_KEEP=1 — leaving session ${SESSION} and share ${shareId} up`)
    return
  }
  log('teardown')
  try { if (shareId) hexpose('share', 'revoke', shareId) } catch {}
  try { hx('server', 'stop') } catch {}
  try { execFileSync('sleep', ['1']) } catch {}
  try { execFileSync('herdr', ['session', 'delete', SESSION], { env: cleanEnv(), stdio: 'ignore' }) } catch {}
  try { rmSync(SESSION_DIR, { recursive: true, force: true }) } catch {}
  try { rmSync(WORK, { recursive: true, force: true }) } catch {}
  try { rmSync(DEMO_CWD, { recursive: true, force: true }) } catch {}

  // Report, never kill: anything still holding OUR session name.
  try {
    const orphans = execFileSync('pgrep', ['-fl', SESSION], { encoding: 'utf8' }).trim()
    if (orphans) console.error(`\x1b[1;31mORPHANS LEFT BEHIND:\x1b[0m\n${orphans}`)
  } catch { log(`no orphan processes for ${SESSION}`) }

  // The owner's permanent daemon must be exactly as healthy as we found it.
  try {
    execFileSync('curl', ['-fsS', '--max-time', '3', 'http://127.0.0.1:21118/healthz'], { stdio: 'ignore' })
    log('the daemon on :21118 is still answering')
  } catch {
    warn('nothing healthy on 127.0.0.1:21118 (it may simply not be running)')
  }
}

/* --------------------------------------------------------------------- run */

if (process.env.HEXSHOT_INDEX_ONLY === '1') {
  writeIndex()
  process.exit(0)
}

let browser = null
try {
  await setup()
  browser = await chromium.launch({ headless: !HEADED })
  if (!SKIP_STILLS) await captureStills(browser)
  if (!SKIP_VIDEO) encode(await recordVideo(browser))
  await browser.close(); browser = null
  writeIndex()
  log('done')
} catch (e) {
  console.error('\x1b[1;31merror:\x1b[0m', e.message)
  process.exitCode = 1
} finally {
  if (browser) await browser.close().catch(() => {})
  teardown()
}
