/**
 * herdr-expose — the browser half of the real acceptance test.
 *
 * This is deliberately NOT a unit test and NOT a protocol probe. It drives a
 * real Chromium against a real `herdr-expose share --lan`, in front of a real
 * Herdr session holding a real agent, and asserts the things a human would
 * notice: that pairing works, that the tree is readable, that the terminal
 * shows what the agent said, that typing into the browser reaches the pane,
 * and — the one that matters most — that a pane left IDLE for a minute still
 * streams when it finally produces output.
 *
 * It is driven by e2e/acceptance.sh, which owns the throwaway session, the
 * share and the teardown. Run it through that; running it bare requires every
 * HEX_* variable below to already be true.
 *
 *   HEX_URL           base URL of the share            (http://ip:port)
 *   HEX_PAIR          a fresh single-use pairing code
 *   HEX_TOKEN         the token the agent was asked to reply with
 *   HEX_SESSION       herdr session name (shown in the sidebar)
 *   HEX_AGENT_PANE    pane id holding the agent        (e.g. w1:p1)
 *   HEX_SHELL_PANE    pane id holding a plain shell    (e.g. w1:p2)
 *   HERDR_SOCKET_PATH socket of that session, for read-back verification
 *   HEX_IDLE_SECS     idle soak before the LIVE test   (default 65)
 *   HEX_HEADED=1      watch it happen
 *   HEX_ARTIFACTS     screenshot directory             (default ./artifacts)
 *
 * Exit code is 0 only if every REQUIRED check passed.
 */

import { chromium } from 'playwright'
import { execFileSync, spawn } from 'node:child_process'
import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

const URL_BASE = req('HEX_URL')
const PAIR = req('HEX_PAIR')
const TOKEN = req('HEX_TOKEN')
const SESSION = req('HEX_SESSION')
const AGENT_PANE = process.env.HEX_AGENT_PANE || 'w1:p1'
const SHELL_PANE = process.env.HEX_SHELL_PANE || 'w1:p2'
const BLOCKED_PANE = process.env.HEX_BLOCKED_PANE || 'w1:p3'
const SHELL_TITLE = process.env.HEX_SHELL_TITLE || 'hexe2e-idle'
const BLOCKED_TITLE = process.env.HEX_BLOCKED_TITLE || 'hexe2e-blocked'
const IDLE_SECS = Number(process.env.HEX_IDLE_SECS || 65)
const ART = process.env.HEX_ARTIFACTS || join(process.cwd(), 'artifacts')
const HEADED = process.env.HEX_HEADED === '1'

function req(name) {
  const v = process.env[name]
  if (!v) {
    console.error(`missing ${name} — run this through e2e/acceptance.sh`)
    process.exit(2)
  }
  return v
}

/* ------------------------------------------------------------------ report */

const results = []
function check(name, ok, detail = '', required = true) {
  results.push({ name, ok: !!ok, detail, required })
  const mark = ok ? 'PASS' : required ? 'FAIL' : 'WARN'
  console.log(`[${mark}] ${name}${detail ? ` — ${detail}` : ''}`)
  return !!ok
}
function skip(name, why) {
  results.push({ name, ok: true, detail: `SKIPPED: ${why}`, required: false, skipped: true })
  console.log(`[SKIP] ${name} — ${why}`)
}

/* -------------------------------------------------------------- herdr side */

function herdr(...args) {
  return execFileSync('herdr', args, {
    env: { ...process.env, HERDR_SESSION: SESSION },
    encoding: 'utf8',
    maxBuffer: 8 * 1024 * 1024,
  })
}
function herdrJSON(...args) {
  return JSON.parse(herdr(...args)).result
}
/** Raw visible text of a pane, straight from Herdr — the ground truth. */
function paneText(pane, lines = 60) {
  try {
    return herdr('pane', 'read', pane, '--lines', String(lines))
  } catch (e) {
    return `<<read failed: ${e.message}>>`
  }
}
function agentState(pane) {
  try {
    return herdrJSON('agent', 'get', pane).agent?.agent_status ?? 'unknown'
  } catch {
    return 'unknown'
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

/* ------------------------------------------------------------ browser side */

/** Text the terminal is actually rendering. Requires the DOM renderer. */
async function termText(page) {
  return page.evaluate(() => {
    const rows = document.querySelector('.xterm-rows')
    return rows ? rows.innerText : ''
  })
}

/** Poll the rendered terminal until `needle` shows up. Returns ms, or -1. */
async function waitForTermText(page, needle, timeoutMs) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeoutMs) {
    if ((await termText(page)).includes(needle)) return Date.now() - t0
    await sleep(250)
  }
  return -1
}

/**
 * Open a pane BY ID, not by title or position.
 *
 * Row order changes the moment something is pinned under "Needs you", and a
 * pane's title is whatever its shell last set, so both are unsafe handles in a
 * test. The app's own viewport map says which target it is streaming `live`,
 * which is exactly the question "is the right pane open?".
 */
async function openPane(page, session, paneId, want = 'transcript') {
  const target = `${session}/${paneId}`
  // BY TARGET, never by index or title. Row order is not stable — "Needs you"
  // re-pins a pane the moment it blocks — and a title is whatever the shell
  // last wrote to OSC 0/2. Both were unsafe handles and both bit this suite.
  const row = page.locator(`.row[data-target="${target}"]`).first()

  // On the phone the list is REPLACED by the pane, so step back out first.
  const back = page.locator('.paneview .iconbtn[aria-label="Back to panes"]')
  if (await back.isVisible().catch(() => false)) {
    await back.click().catch(() => {})
    await page.locator('.row').first().waitFor({ state: 'visible', timeout: 5000 }).catch(() => {})
  }
  try {
    await row.click({ timeout: 15000 })
  } catch {
    return false
  }
  await page
    .locator(want === 'transcript' ? '.tr-body' : '.term-host .xterm')
    .waitFor({ state: 'visible', timeout: 20000 })
    .catch(() => {})
  for (let j = 0; j < 24; j++) {
    const vp = (await stats(page))?.viewport ?? {}
    if (vp[target] === want) return true
    await sleep(250)
  }
  return false
}

/**
 * Flip the header view toggle and wait for the server-declared mode to follow.
 * A pane with an agent now DEFAULTS to transcript, so every terminal assertion
 * has to ask for the terminal explicitly.
 */
async function setView(page, session, paneId, kind) {
  const target = `${session}/${paneId}`
  const want = kind === 'transcript' ? 'transcript' : 'live'
  await page.locator(`.viewtoggle-btn:text-is("${kind === 'transcript' ? 'text' : 'term'}")`).click()
  // The terminal is opt-in: the first tap on a pane asks what it costs and
  // waits. Answering is part of switching view, so the helper answers.
  if (kind === 'terminal') {
    const confirm = page.locator('.notice-confirm button:text-is("Show terminal")')
    if (await confirm.isVisible().catch(() => false)) await confirm.click()
  }
  for (let i = 0; i < 24; i++) {
    const vp = (await stats(page))?.viewport ?? {}
    if (vp[target] === want) return true
    await sleep(250)
  }
  return false
}

/** Text the transcript view is actually rendering. */
async function transcriptText(page) {
  return page.evaluate(() => document.querySelector('.tr-text')?.innerText ?? '')
}

async function waitForTranscriptText(page, needle, timeoutMs) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeoutMs) {
    if ((await transcriptText(page)).includes(needle)) return Date.now() - t0
    await sleep(300)
  }
  return -1
}

/**
 * `viewport_rows` for a pane, straight from Herdr. This is the number
 * AMENDMENTS 13 is about: attaching a live observer changes it (and SIGWINCHes
 * the agent into throwing its screen away); a transcript subscriber must not.
 */
function viewportRows(pane) {
  try {
    const panes = herdrJSON('pane', 'list').panes ?? []
    return panes.find((p) => p.pane_id === pane)?.scroll?.viewport_rows ?? null
  } catch {
    return null
  }
}

/**
 * THE NON-MUTATION BASELINE.
 *
 * Everything the browser could disturb about the owner's session, in one
 * comparable object: each pane's PTY rows (`scroll.viewport_rows` — verified to
 * track the real PTY, moving 40 -> 60 when an external controller resized it),
 * each pane's scrollback position, and which pane herdr considers focused.
 *
 * "Looking must not touch" is only a claim until this is captured before and
 * after and found identical.
 */
function paneState() {
  try {
    const panes = herdrJSON('pane', 'list').panes ?? []
    const out = { focused: null, panes: {} }
    for (const p of panes) {
      out.panes[p.pane_id] = {
        viewport_rows: p.scroll?.viewport_rows ?? null,
        offset_from_bottom: p.scroll?.offset_from_bottom ?? null,
      }
      if (p.focused) out.focused = p.pane_id
    }
    return out
  } catch (e) {
    return { error: String(e) }
  }
}

/**
 * Assert that nothing the owner would notice has moved.
 *
 * `label` names the thing the browser just did, so a failure reads as
 * "switching panes resized w1:p2" rather than as a diff of two blobs.
 */
function checkUntouched(label, before, after = paneState()) {
  const diffs = []
  if (before.focused !== after.focused)
    diffs.push(`focused pane ${before.focused} -> ${after.focused}`)
  for (const [id, b] of Object.entries(before.panes ?? {})) {
    const a = after.panes?.[id]
    if (!a) {
      diffs.push(`${id} vanished`)
      continue
    }
    if (b.viewport_rows !== a.viewport_rows)
      diffs.push(`${id} viewport_rows ${b.viewport_rows} -> ${a.viewport_rows}`)
    if (b.offset_from_bottom !== a.offset_from_bottom)
      diffs.push(`${id} scroll.offset_from_bottom ${b.offset_from_bottom} -> ${a.offset_from_bottom}`)
  }
  check(
    `LOOKING DOES NOT TOUCH: ${label} leaves every pane's rows, scroll and focus alone`,
    diffs.length === 0,
    diffs.length === 0
      ? `checked ${Object.keys(before.panes ?? {}).length} panes, focused=${after.focused}`
      : diffs.join('; '),
  )
  return after
}

/**
 * The PTY's real size, asked of the shell itself.
 *
 * `pane list` reports rows but no COLS anywhere in herdr 0.9.0, and cols are
 * exactly what a browser window used to impose. So we ask the shell: `tput`
 * reads the tty, which is the ground truth nothing can fake.
 */
async function ptySize(pane, tag) {
  try {
    herdr('pane', 'send-text', pane, `echo ${tag}=$(tput cols)x$(tput lines)`)
    await sleep(300)
    herdr('pane', 'send-keys', pane, 'enter')
  } catch {
    return null
  }
  const re = new RegExp(`${tag}=(\\d+)x(\\d+)`)
  for (let i = 0; i < 25; i++) {
    await sleep(400)
    const m = re.exec(paneText(pane, 40))
    if (m) return `${m[1]}x${m[2]}`
  }
  return null
}

async function stats(page) {
  return page.evaluate(() => (window.__herdrStats ? window.__herdrStats() : null))
}

/**
 * Pixel difference between two PNG buffers, computed inside the page itself so
 * the harness needs no image dependency. Returns the fraction of differing
 * pixels (0..1), or -1 when the two shots are not the same size.
 */
async function pixelDiff(page, a, b) {
  const toURL = (buf) => 'data:image/png;base64,' + buf.toString('base64')
  return page.evaluate(
    async ([ua, ub]) => {
      const load = async (u) => createImageBitmap(await (await fetch(u)).blob())
      const [ia, ib] = await Promise.all([load(ua), load(ub)])
      if (ia.width !== ib.width || ia.height !== ib.height) return -1
      const draw = (img) => {
        const c = new OffscreenCanvas(img.width, img.height)
        const x = c.getContext('2d')
        x.drawImage(img, 0, 0)
        return x.getImageData(0, 0, img.width, img.height).data
      }
      const da = draw(ia)
      const db = draw(ib)
      let diff = 0
      for (let i = 0; i < da.length; i += 4) {
        if (da[i] !== db[i] || da[i + 1] !== db[i + 1] || da[i + 2] !== db[i + 2]) diff++
      }
      return diff / (ia.width * ia.height)
    },
    [toURL(a), toURL(b)],
  )
}

/* --------------------------------------------------------------------- run */

const main = async () => {
  mkdirSync(ART, { recursive: true })
  const browser = await chromium.launch({ headless: !HEADED })
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } })

  /**
   * Force xterm's DOM renderer before the app boots. WebGL and Canvas draw the
   * terminal into a <canvas>, which a test cannot read back as text. The app
   * already supports this fallback and persists it exactly here, so we are
   * using a shipped code path, not a hook added for the test.
   */
  await ctx.addInitScript(() => {
    try {
      const ua = navigator.userAgent.slice(0, 200)
      const at = Date.now()
      localStorage.setItem(
        'herdr-expose.renderer-blocklist',
        JSON.stringify([
          { ua, kind: 'webgl', at },
          { ua, kind: 'canvas', at },
        ]),
      )
    } catch {}
  })

  const page = await ctx.newPage()
  const consoleErrors = []
  page.on('console', (m) => {
    if (m.type() === 'error') consoleErrors.push(m.text())
  })
  page.on('pageerror', (e) => consoleErrors.push(String(e)))

  const shot = async (name, target) =>
    (target ?? page).screenshot({ path: join(ART, `${name}.png`) })

  /* -- 1. unpaired browsers get the pairing screen ----------------------- */
  await page.goto(URL_BASE, { waitUntil: 'domcontentloaded' })
  const pairVisible = await page
    .locator('.pair-input')
    .waitFor({ state: 'visible', timeout: 15000 })
    .then(() => true)
    .catch(() => false)
  await shot('01-pair-screen')
  check('unpaired device is stopped at the pairing screen', pairVisible)

  /* -- 2. the ?pair= deep link gets past it ------------------------------ */
  await page.goto(`${URL_BASE}/?pair=${PAIR}`, { waitUntil: 'domcontentloaded' })
  const paired = await page
    .locator('.panelist')
    .waitFor({ state: 'visible', timeout: 20000 })
    .then(() => true)
    .catch(() => false)
  check('the ?pair= deep link pairs the device', paired)
  const urlAfter = page.url()
  check(
    'the pairing code is scrubbed from the URL',
    !urlAfter.includes('pair='),
    urlAfter,
    false,
  )
  await shot('02-paired-tree')

  if (!paired) {
    await browser.close()
    return finish()
  }

  /* -- 3. the tree is human-readable ------------------------------------- */
  const sidebarText = await page.locator('.sidebar').innerText()
  const sessionNames = await page.locator('.session-name').allInnerTexts()
  const rowTitles = await page.locator('.sidebar .row-title').allInnerTexts()
  check(
    'the session appears in the sidebar under its own name',
    // innerText is the CSS-transformed text and the header is uppercased.
    sessionNames.some((n) => n.toLowerCase() === SESSION.toLowerCase()),
    `saw [${sessionNames.join(', ')}]`,
  )
  check(
    'the panes appear with human titles',
    rowTitles.length >= 2 && rowTitles.every((t) => t.trim().length > 0),
    `${rowTitles.length} rows: ${JSON.stringify(rowTitles)}`,
  )
  const rawIds = sidebarText.match(/\bw\d+:[tp]\d+\b/g) || []
  check('no raw pane/tab ids are rendered as labels', rawIds.length === 0, rawIds.join(' '))
  // A pane the user has NAMED should win over whatever its shell last set as
  // the terminal title. herdr-expose has no `label` field at all today.
  check(
    'a user-set pane name (`herdr pane rename`) reaches the browser',
    rowTitles.some((t) => t.includes(SHELL_TITLE)),
    `looked for "${SHELL_TITLE}" in ${JSON.stringify(rowTitles)}`,
    false,
  )

  /* -- 4. LOOKING MUST NOT TOUCH ---------------------------------------- */
  //
  // The owner, working at his laptop while a browser was open on the same
  // session: "you are scrolling actual herdr terminal."
  //
  // So the baseline is taken BEFORE the browser looks at anything, and every
  // step below is checked against it. What is compared is exactly what he
  // would notice: each pane's PTY rows, each pane's scrollback position, and
  // which pane herdr thinks is focused.
  const UNTOUCHED = paneState()
  check(
    'herdr gives us a full pane baseline to hold the browser to',
    !UNTOUCHED.error && Object.keys(UNTOUCHED.panes).length >= 3,
    JSON.stringify(UNTOUCHED),
  )
  // Cols are the number a browser window used to impose and that herdr reports
  // nowhere, so we ask the shell's own tty for them.
  const ptyBefore = await ptySize(SHELL_PANE, 'PTYA')
  check('the shell pane reports its real PTY size', !!ptyBefore, `tput says ${ptyBefore}`)

  const rowsBefore = viewportRows(AGENT_PANE)
  check(
    'herdr reports a viewport_rows for the agent pane before we attach',
    typeof rowsBefore === 'number' && rowsBefore > 0,
    `viewport_rows=${rowsBefore}`,
  )

  const trOpen = await openPane(page, SESSION, AGENT_PANE, 'transcript')
  check(
    `an agent pane (${AGENT_PANE}) opens in TRANSCRIPT view by default`,
    trOpen,
    `viewport declared ${JSON.stringify((await stats(page))?.viewport ?? {})}`,
  )
  await sleep(3000)
  await shot('03a-transcript')

  const rowsAfter = viewportRows(AGENT_PANE)
  check(
    'THE POINT: attaching in transcript view does NOT resize the pane',
    rowsBefore !== null && rowsAfter === rowsBefore,
    `viewport_rows ${rowsBefore} -> ${rowsAfter}`,
  )
  checkUntouched('opening a pane in the browser', UNTOUCHED)
  const trStats = (await stats(page))?.targets?.[`${SESSION}/${AGENT_PANE}`] ?? {}
  check(
    'and the client sent no `resize` for it at all',
    (trStats.resizeSent ?? 0) === 0,
    `resizeSent=${trStats.resizeSent ?? 0}`,
  )

  const trText = await transcriptText(page)
  check(
    'the transcript renders readable text from the agent pane',
    trText.trim().length > 20,
    `${trText.trim().length} chars: ${JSON.stringify(trText.trim().slice(0, 80))}`,
  )
  check(
    'the transcript carries no escape sequences (stripped server-side)',
    // eslint-disable-next-line no-control-regex
    !/\u001b/.test(trText),
    'searched the rendered text for ESC',
  )
  const prov = await page.locator('.tr-provenance').innerText().catch(() => '')
  check(
    'the transcript says plainly that it is a SCREEN, not a conversation log',
    /screen/i.test(prov) && /not a reconstructed conversation/i.test(prov),
    JSON.stringify(prov.slice(0, 140)),
  )
  check(
    'and it is honest about not resizing the pane',
    /does not resize/i.test(prov),
    JSON.stringify(prov.slice(-60)),
  )

  /* -- 4b. the prompt box round-trips to the agent ----------------------- */
  const trToken = `HEXE2E-TR-${Math.random().toString(16).slice(2, 8)}`
  await page.locator('.tr-input').fill(`Reply with exactly this text and nothing else: ${trToken}`)
  await page.locator('.tr-send').click()
  let trArrived = -1
  for (let i = 0; i < 40; i++) {
    if (paneText(AGENT_PANE, 200).includes(trToken)) {
      trArrived = i * 500
      break
    }
    await sleep(500)
  }
  check(
    'the transcript prompt box sends a real prompt to the agent',
    trArrived >= 0,
    trArrived >= 0
      ? `"${trToken}" reached ${AGENT_PANE} in ~${trArrived}ms (verified with \`herdr pane read\`)`
      : `"${trToken}" never reached the pane`,
  )
  const trEcho = await waitForTranscriptText(page, trToken, 45000)
  check(
    'and the transcript shows it back within a couple of poll ticks',
    trEcho >= 0,
    trEcho >= 0 ? `visible after ${trEcho}ms` : 'never appeared in 45s',
  )
  const rowsAfterPrompt = viewportRows(AGENT_PANE)
  check(
    'prompting from the transcript still does not resize the pane',
    rowsAfterPrompt === rowsBefore,
    `viewport_rows ${rowsBefore} -> ${rowsAfterPrompt}`,
  )
  // A prompt is the user acting, and it is still not allowed to resize or
  // scroll anything: agent.prompt carries no geometry and takes no control.
  checkUntouched('sending a prompt from the transcript', UNTOUCHED)

  /* -- 4c. the terminal is OPT-IN, and even then does not resize --------- */
  // Terminal is still available on any pane and is still the right tool when
  // you need exactness — but it now costs one deliberate answer, and when it
  // does attach it renders at the PANE'S size rather than this window's.
  await page.locator('.viewtoggle-btn:text-is("term")').click()
  const confirmBar = page.locator('.notice-confirm')
  const asked = await confirmBar
    .waitFor({ state: 'visible', timeout: 8000 })
    .then(() => true)
    .catch(() => false)
  const confirmText = asked ? await confirmBar.innerText() : ''
  check('tapping `term` ASKS before attaching, on this pane, once', asked, confirmText.slice(0, 120))
  check(
    'and the question says what it costs and what text view does not',
    /resize/i.test(confirmText) && /text view/i.test(confirmText),
    JSON.stringify(confirmText.slice(0, 200)),
  )
  check(
    'declining leaves the pane in text view and nothing attached',
    await (async () => {
      await page.locator('.notice-confirm button:text-is("Stay in text")').click()
      await sleep(600)
      const vp = (await stats(page))?.viewport ?? {}
      return vp[`${SESSION}/${AGENT_PANE}`] === 'transcript'
    })(),
  )
  checkUntouched('tapping `term` and declining', UNTOUCHED)

  check(
    `the header toggle switches the agent pane to the terminal and it goes live`,
    await setView(page, SESSION, AGENT_PANE, 'terminal'),
  )
  await sleep(3000)
  // THE HEADLINE REVERSAL. Terminal view used to send `resize` on attach, which
  // herdr applies to the pane for EVERY client on it — that is the reflow the
  // owner felt under his hands. It now attaches with no geometry at all.
  checkUntouched('switching to TERMINAL view at 1280x900', UNTOUCHED)
  const geomLine = await page.locator('.paneview-geom').innerText().catch(() => '')
  check(
    'the terminal says what size it is rendering at, and whose size it is',
    /pane's own size/i.test(geomLine) && /\d+x\d+/.test(geomLine),
    JSON.stringify(geomLine),
  )
  check(
    'the client sent no `resize` for the agent pane at any point',
    ((await stats(page))?.targets?.[`${SESSION}/${AGENT_PANE}`]?.resizeSent ?? 0) === 0,
    `resizeSent=${(await stats(page))?.targets?.[`${SESSION}/${AGENT_PANE}`]?.resizeSent ?? 0}`,
  )
  await page
    .locator('.term-host .xterm')
    .waitFor({ state: 'visible', timeout: 20000 })
    .catch(() => {})
  await sleep(2500)
  const promptProc = spawn(
    'herdr',
    ['agent', 'prompt', AGENT_PANE, `Reply with exactly this text and nothing else: ${TOKEN}`],
    { env: { ...process.env, HERDR_SESSION: SESSION }, stdio: 'ignore' },
  )
  const tokenMs = await waitForTermText(page, TOKEN, 120000)
  promptProc.kill('SIGTERM')
  await shot('03-terminal-with-token')
  check(
    `the terminal renders the agent's reply (${TOKEN}) as it is produced`,
    tokenMs >= 0,
    tokenMs >= 0 ? `visible after ${tokenMs}ms` : 'never appeared in 120s',
  )
  check(
    'and Herdr agrees the pane really contains it',
    paneText(AGENT_PANE).includes(TOKEN),
    'cross-checked with `herdr pane read`',
  )

  /* -- 4d. the view choice is remembered per pane ------------------------ */
  const remembered = await page.evaluate(
    (t) => localStorage.getItem('hex.paneview.' + t),
    `${SESSION}/${AGENT_PANE}`,
  )
  check(
    'the view toggle is remembered per pane',
    remembered === 'terminal',
    `stored ${JSON.stringify(remembered)}`,
  )
  const consent = await page.evaluate(
    (t) => sessionStorage.getItem('hex.termok.' + t),
    `${SESSION}/${AGENT_PANE}`,
  )
  check(
    'and the terminal answer is remembered too — it does not nag again this session',
    consent === '1',
    `stored ${JSON.stringify(consent)}`,
  )

  /* -- 5. keystrokes typed in the browser reach the pane ----------------- */
  const typed = `hexe2e-typed-${Math.random().toString(16).slice(2, 8)}`
  await page.locator('.term-host').click()
  await page.keyboard.type(typed, { delay: 25 })
  let arrived = -1
  for (let i = 0; i < 40; i++) {
    if (paneText(AGENT_PANE).includes(typed)) {
      arrived = i * 250
      break
    }
    await sleep(250)
  }
  await shot('04-typed-from-browser')
  check(
    'typing in the browser reaches the pane (verified with `herdr pane read`)',
    arrived >= 0,
    arrived >= 0 ? `"${typed}" landed in ${AGENT_PANE} within ~${arrived}ms` : `"${typed}" never arrived`,
  )
  // Leave the agent's prompt box as we found it.
  await page.keyboard.press('Control+u').catch(() => {})
  await sleep(2000)
  // TAKING CONTROL MUST NOT ALSO RESIZE. Typing upgrades the stream from
  // `observe` to `control --takeover`, and that is the one call in this program
  // that really can change the owner's pane — measured on 0.9.0, a controller
  // attaching at 100x60 moved the pane from 120x40 and left it there. It now
  // reuses the size herdr already told us the pane is.
  checkUntouched('TAKING CONTROL by typing in the browser', UNTOUCHED)

  /* -- 6. the agent badge reflects reality ------------------------------- */
  // Both sides move: wait for agreement rather than sampling a race. A badge
  // that never catches up inside 20s is the bug this is looking for.
  let badgeState = null
  let truth = null
  const t0badge = Date.now()
  while (Date.now() - t0badge < 20000) {
    truth = agentState(AGENT_PANE)
    badgeState = await page
      .locator('.paneview .badge')
      .first()
      .getAttribute('data-state')
      .catch(() => null)
    if (badgeState === truth) break
    await sleep(500)
  }
  check(
    'the agent state badge matches what Herdr reports',
    badgeState === truth,
    `browser=${badgeState} herdr=${truth} after ${Date.now() - t0badge}ms`,
  )
  // Watch it move: drive the agent and require the badge to report `working`.
  const busy = spawn('herdr', ['agent', 'prompt', AGENT_PANE, 'Count slowly from 1 to 20, one number per line.'],
    { env: { ...process.env, HERDR_SESSION: SESSION }, stdio: 'ignore' })
  let sawWorking = false
  for (let i = 0; i < 60; i++) {
    const b = await page.locator('.paneview .badge').first().getAttribute('data-state').catch(() => null)
    if (b === 'working') { sawWorking = true; break }
    await sleep(500)
  }
  check('the badge goes to `working` while the agent works', sawWorking)
  busy.kill('SIGTERM')

  /* -- 7. a blocked agent shows its question ----------------------------- */
  // Driven through `herdr pane report-agent`, the documented API an agent uses
  // to declare its own lifecycle state. Baiting a real coding agent into an
  // approval dialog is not reproducible (it depends on that agent's permission
  // settings); what herdr-expose owes us is that when Herdr SAYS blocked, the
  // browser pins the pane, flips the badge and shows the question.
  const QUESTION = 'Do you want to proceed with the e2e acceptance check?'
  let blockedSeen = false
  try {
    herdr(
      'pane', 'report-agent', BLOCKED_PANE,
      '--source', 'hexe2e-test', '--agent', 'claude', '--state', 'blocked',
      '--message', QUESTION,
    )
    for (let i = 0; i < 20; i++) {
      if (agentState(BLOCKED_PANE) === 'blocked') {
        blockedSeen = true
        break
      }
      await sleep(500)
    }
  } catch (e) {
    console.log(`  (could not report a blocked state: ${e.message})`)
  }
  if (blockedSeen) {
    // Exactly one pane is blocked, so the urgent section has exactly one row.
    const urgentRow = page.locator('.ws-urgent .row').first()
    const pinned = await urgentRow
      .waitFor({ state: 'visible', timeout: 15000 })
      .then(() => true)
      .catch(() => false)
    check('a blocked pane is pinned under "Needs you"', pinned)
    // A pane with an agent now opens as a TRANSCRIPT, so the blocked question
    // arrives through the transcript (read from herdr's own `detection`
    // region) rather than through the terminal's Q&A panel.
    const rowsBlockedBefore = viewportRows(BLOCKED_PANE)
    if (pinned) await urgentRow.click()
    const trBody = page.locator('.tr-body')
    const shown = await trBody
      .waitFor({ state: 'visible', timeout: 15000 })
      .then(() => true)
      .catch(() => false)
    // The transcript is POLLED at ~1Hz, so give it a couple of ticks rather
    // than sampling the frame before the first one lands.
    let qaText = ''
    for (let i = 0; i < 20 && shown; i++) {
      qaText = await transcriptText(page)
      if (qaText.trim().length > 0) break
      await sleep(500)
    }
    await shot('05-blocked-question')
    // Rendered VERBATIM from herdr's own `detection` region — the same region
    // Herdr classifies on, so the transcript and the answer keys can never be
    // looking at different things. It is whatever the pane actually printed:
    // `pane report-agent --message` sets metadata, it does not print, so the
    // assertion is that the pane's own prompt region is shown, not that our
    // message text appears in it.
    check(
      'a blocked agent shows its prompt region in the browser',
      shown && qaText.trim().length > 0,
      shown ? JSON.stringify(qaText.slice(0, 160)) : 'no transcript appeared',
    )
    check(
      'the transcript says it is reading the prompt region while blocked',
      /prompt region/i.test(await page.locator('.tr-provenance').innerText().catch(() => '')),
      'checked the provenance line',
    )
    const badge2 = await page.locator('.paneview .badge').first().getAttribute('data-state')
    check('the badge flips to blocked', badge2 === 'blocked', `badge=${badge2}`)
    const keys = await page.locator('.tr-keys .key').allInnerTexts()
    check(
      'the answer key bar is offered (y / n / 1 / 2 / 3, arrows, esc, enter)',
      ['y', 'n', '1', '2', '3', 'esc', '↑', '↓', '←', '→'].every((k) => keys.includes(k)),
      JSON.stringify(keys),
    )
    check(
      'and reading a BLOCKED agent did not resize it either',
      viewportRows(BLOCKED_PANE) === rowsBlockedBefore,
      `viewport_rows ${rowsBlockedBefore} -> ${viewportRows(BLOCKED_PANE)}`,
    )
    herdr('pane', 'report-agent', BLOCKED_PANE,
      '--source', 'hexe2e-test', '--agent', 'claude', '--state', 'idle')
  } else {
    skip('a blocked agent shows its question', 'Herdr never reported the pane as blocked')
  }

  /* -- 8. THE ONE THAT MATTERS: an idle pane still streams --------------- */
  // A plain shell, not the agent: an agent TUI repaints on its own and would
  // mask exactly the bug this is here to catch. A bash prompt emits nothing.
  const shellOpen = await openPane(page, SESSION, SHELL_PANE, 'transcript')
  check(`the idle pane (${SHELL_PANE}) opens`, shellOpen)
  // EVERY pane opens as a transcript now, including a plain shell. A shell is
  // output like any other output, and reading it is the one thing that cannot
  // disturb the person typing into it.
  check(
    'a pane with NO agent also defaults to the TEXT view',
    await page.locator('.tr-body').isVisible().catch(() => false),
    'no stored preference for this pane',
  )
  checkUntouched(`switching panes (to ${SHELL_PANE})`, UNTOUCHED)
  // Now ask for the terminal explicitly, which is what the rest of section 8
  // measures (flicker, render health, the idle stream).
  check(
    `the idle pane goes live once the terminal is asked for`,
    await setView(page, SESSION, SHELL_PANE, 'terminal'),
  )
  // A browser-window resize, which is what a real client does when the layout
  // moves. It must change what WE render and nothing upstream.
  await sleep(800)
  await page.setViewportSize({ width: 1240, height: 860 })
  await sleep(800)
  await page.setViewportSize({ width: 1280, height: 900 })
  await sleep(2000)
  checkUntouched('resizing the BROWSER window while a terminal is open', UNTOUCHED)

  const statsBefore = await stats(page)
  const shotA = await page.locator('.term-wrap').screenshot()
  await sleep(2000)
  const shotB = await page.locator('.term-wrap').screenshot()
  const diff = await pixelDiff(page, shotA, shotB)
  writeFileSync(join(ART, '06-settled-a.png'), shotA)
  writeFileSync(join(ART, '06-settled-b.png'), shotB)
  check(
    'a settled terminal does not flicker (2s apart, pixel diff)',
    diff >= 0 && diff < 0.02,
    diff < 0 ? 'screenshots differ in size' : `${(diff * 100).toFixed(3)}% of pixels changed`,
  )

  /* -- 8b. the render-health watchdog repairs, ONCE ---------------------- */
  //
  // The terminal view is pinned to a character grid, and SPEC B8 is a list of
  // ways that pinning silently comes undone. The client must notice and fix
  // itself — the owner is not the misalignment detector. But a watchdog that
  // thrashes hands back every round of flicker we killed, so the assertion is
  // deliberately two-sided: it repairs, and it repairs EXACTLY ONCE.
  const healthBefore = (await stats(page))?.health?.counts ?? {}
  const disturbed = await page.evaluate(() =>
    window.__herdrDisturbGrid ? window.__herdrDisturbGrid(13) : null,
  )
  check(
    'the harness can put the grid out of alignment with its container',
    !!disturbed,
    JSON.stringify(disturbed),
  )
  const detected = await page.evaluate(() =>
    window.__herdrCheckRender ? window.__herdrCheckRender() : null,
  )
  check(
    'the client DETECTS that it is rendering at the wrong geometry',
    !!detected && detected.reasons?.includes('geometry-drift'),
    JSON.stringify(detected?.reasons ?? null),
  )

  // Give the watchdog its tick, plus margin. No forcing: it has to act on its
  // own schedule or it is not a watchdog.
  let repaired = -1
  for (let i = 0; i < 30; i++) {
    await sleep(500)
    const r = await page.evaluate(() =>
      window.__herdrCheckRender ? window.__herdrCheckRender() : 'gone',
    )
    if (r === null) {
      repaired = i * 500
      break
    }
  }
  check(
    'and REPAIRS it without anyone being told',
    repaired >= 0,
    repaired >= 0 ? `back in alignment after ~${repaired}ms` : 'still misaligned after 15s',
  )
  const healthAfter = (await stats(page))?.health?.counts ?? {}
  const repairs = (healthAfter.repair ?? 0) - (healthBefore.repair ?? 0)
  check(
    'exactly one repair, not a storm',
    repairs === 1,
    `repair ${healthBefore.repair ?? 0} -> ${healthAfter.repair ?? 0} (delta ${repairs})`,
  )
  check(
    'the repair did not need to reset the emulator',
    (healthAfter.repaint ?? 0) === (healthBefore.repaint ?? 0),
    `repaint delta ${(healthAfter.repaint ?? 0) - (healthBefore.repaint ?? 0)}`,
  )
  const stuck = await page.locator('.notice-health').isVisible().catch(() => false)
  check('no "display out of sync" notice after a successful repair', !stuck)
  // Let it settle again before the idle soak measures churn.
  await sleep(1500)

  console.log(`  ... holding an IDLE pane open for ${IDLE_SECS}s ...`)
  await sleep(IDLE_SECS * 1000)

  const statsMid = await stats(page)
  const tgtBefore = statsBefore?.targets?.[`${SESSION}/${SHELL_PANE}`] ?? {}
  const tgtMid = statsMid?.targets?.[`${SESSION}/${SHELL_PANE}`] ?? {}
  check(
    `no terminal churn over the ${IDLE_SECS}s idle window (reset/termCreate stay put)`,
    (tgtMid.reset ?? 0) === (tgtBefore.reset ?? 0) &&
      (tgtMid.termCreate ?? 0) === (tgtBefore.termCreate ?? 0),
    `reset ${tgtBefore.reset ?? 0}->${tgtMid.reset ?? 0}, termCreate ${tgtBefore.termCreate ?? 0}->${tgtMid.termCreate ?? 0}`,
  )
  check(
    'termCreate is exactly 1 for this pane',
    (tgtMid.termCreate ?? 0) === 1,
    `termCreate=${tgtMid.termCreate ?? 0}`,
  )
  // The soak is also a non-mutation test: a pane left open for a minute must
  // not drift the owner's terminal a cell.
  checkUntouched(`leaving a terminal open for ${IDLE_SECS}s`, UNTOUCHED)

  const token2 = `HEXE2E-LIVE-${Math.random().toString(16).slice(2, 8)}`
  herdr('pane', 'send-text', SHELL_PANE, `echo ${token2}`)
  await sleep(200)
  herdr('pane', 'send-keys', SHELL_PANE, 'enter')
  const liveMs = await waitForTermText(page, token2, 30000)
  await shot('07-idle-then-output')
  check(
    `output produced after ${IDLE_SECS}s of idle still reaches the browser`,
    liveMs >= 0,
    liveMs >= 0
      ? `appeared after ${liveMs}ms`
      : `${token2} NEVER appeared in 30s — the LIVE stream died on the idle pane`,
  )
  if (liveMs < 0) {
    writeFileSync(
      join(ART, 'idle-bug-evidence.txt'),
      [
        `token: ${token2}`,
        `pane:  ${SHELL_PANE}`,
        '--- what the pane really contains, per herdr ---',
        paneText(SHELL_PANE),
        '--- what the browser is rendering ---',
        await termText(page),
        '--- stats ---',
        JSON.stringify(await stats(page), null, 2),
      ].join('\n'),
    )
  }

  const statsAfter = await stats(page)
  writeFileSync(join(ART, 'stats.json'), JSON.stringify(statsAfter, null, 2))

  /* -- 9. 375px, the design target --------------------------------------- */
  await page.setViewportSize({ width: 375, height: 812 })
  await page.reload({ waitUntil: 'domcontentloaded' })
  const mobileShell = await page
    .locator('.app-mobile')
    .waitFor({ state: 'visible', timeout: 15000 })
    .then(() => true)
    .catch(() => false)
  check('375px gets the mobile shell, not a shrunken desktop', mobileShell)
  const mobileRows = await page.locator('.row-title').allInnerTexts()
  check('the pane list is usable at 375px', mobileRows.length >= 2, `${mobileRows.length} rows`)
  await shot('08-mobile-list')
  const mobileOpen = await openPane(page, SESSION, SHELL_PANE, 'live')
  check('the idle pane opens at 375px', mobileOpen)
  // It opens in the TERMINAL because this device already chose that for this
  // pane and already answered the question — the toggle is sticky per pane and
  // the consent is sticky for the session, so a phone does not re-litigate a
  // decision made thirty seconds ago on the same tab.
  const mobileTerm = await page
    .locator('.term-host .xterm')
    .waitFor({ state: 'visible', timeout: 20000 })
    .then(() => true)
    .catch(() => false)
  await shot('09-mobile-terminal')
  check('the terminal renders at 375px', mobileTerm)
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth,
  )
  check('nothing overflows horizontally at 375px', overflow <= 1, `${overflow}px of overflow`)

  /* -- 9b. the transcript at 375px, which is what it is FOR -------------- */
  await page.locator('.paneview .iconbtn[aria-label="Back to panes"]').click().catch(() => {})
  await page.locator('.row').first().waitFor({ state: 'visible', timeout: 8000 }).catch(() => {})
  // Section 4c switched this pane to the terminal and the app REMEMBERED that
  // (asserted above). Forget it, so what follows tests the DEFAULT.
  await page.evaluate(
    (t) => localStorage.removeItem('hex.paneview.' + t),
    `${SESSION}/${AGENT_PANE}`,
  )
  // Take a FRESH baseline. Section 4c deliberately opened this pane in the
  // TERMINAL at 1280x900, which attaches an observer and legitimately does
  // resize the PTY — that contrast is the whole argument for transcript being
  // the default, and it makes the earlier number stale here.
  const rowsBeforePhone = viewportRows(AGENT_PANE)
  const mobileTr = await openPane(page, SESSION, AGENT_PANE, 'transcript')
  check('the agent pane opens in transcript view at 375px', mobileTr)
  await sleep(2500)
  await shot('10-mobile-transcript')
  const mobileTrText = await transcriptText(page)
  check(
    'the transcript is readable at 375px',
    mobileTrText.trim().length > 20,
    `${mobileTrText.trim().length} chars`,
  )
  const trOverflow = await page.evaluate(() => {
    const doc = document.documentElement.scrollWidth - window.innerWidth
    const body = document.querySelector('.tr-body')
    const inner = body ? body.scrollWidth - body.clientWidth : 0
    return { doc, inner }
  })
  check(
    'the transcript wraps to the VIEWPORT — no sideways scroll at 375px',
    trOverflow.doc <= 1 && trOverflow.inner <= 1,
    `page ${trOverflow.doc}px, transcript ${trOverflow.inner}px`,
  )
  const rowsMobile = viewportRows(AGENT_PANE)
  check(
    'and opening it on a PHONE still does not resize the pane',
    rowsBeforePhone !== null && rowsMobile === rowsBeforePhone,
    `viewport_rows ${rowsBeforePhone} -> ${rowsMobile} at 375px`,
  )
  // THE CLAIM, STATED AS A MEASUREMENT. This assertion used to read "for
  // contrast: the terminal view earlier in this run DID resize it" — the
  // suite proved the destruction and shipped it as a documented contrast.
  // It is now inverted: the terminal view at 1280x900, the phone at 375px,
  // typing, prompting and a minute of idle are all in this run, and the
  // owner's pane came out of all of it unchanged.
  check(
    'NOTHING in this whole browser session resized the agent pane',
    rowsBefore !== null && rowsBeforePhone === rowsBefore && rowsMobile === rowsBefore,
    `viewport_rows ${rowsBefore} (before any attach) -> ${rowsBeforePhone} (after the terminal view at 1280x900) -> ${rowsMobile} (at 375px)`,
  )
  checkUntouched('the entire browser session, end to end', UNTOUCHED)
  // And the number herdr never reports: the shell's own tty, asked directly.
  const ptyAfter = await ptySize(SHELL_PANE, 'PTYZ')
  check(
    "the shell pane's REAL PTY size (tput, its own tty) is byte-for-byte what we found",
    !!ptyBefore && ptyAfter === ptyBefore,
    `tput ${ptyBefore} -> ${ptyAfter}`,
  )

  check(
    'no uncaught console errors',
    consoleErrors.length === 0,
    consoleErrors.slice(0, 3).join(' | '),
    false,
  )

  await browser.close()
  finish()
}

function finish() {
  const failed = results.filter((r) => r.required && !r.ok)
  console.log('\n' + '='.repeat(72))
  console.log(
    `${results.filter((r) => r.ok && !r.skipped).length} passed, ${failed.length} failed, ` +
      `${results.filter((r) => r.skipped).length} skipped`,
  )
  if (failed.length) {
    console.log('\nFAILED:')
    for (const f of failed) console.log(`  - ${f.name}${f.detail ? ` (${f.detail})` : ''}`)
  }
  console.log(`artifacts: ${ART}`)
  writeFileSync(join(ART, 'results.json'), JSON.stringify(results, null, 2))
  process.exit(failed.length ? 1 : 0)
}

main().catch((e) => {
  console.error('harness crashed:', e)
  check('harness completed', false, String(e))
  finish()
})
