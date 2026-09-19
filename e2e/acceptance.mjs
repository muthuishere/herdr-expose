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
async function openPane(page, session, paneId, want = 'live') {
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

  /* -- 4. TRANSCRIPT: the default for an agent pane, and non-destructive -- */
  //
  // This is the headline property of AMENDMENTS 13. A live observer attaches a
  // PTY at the browser's size, which SIGWINCHes an agent TUI into discarding
  // its screen — so opening a pane on a phone used to destroy the history you
  // opened it to read. A transcript subscriber declares no geometry at all, and
  // `viewport_rows` is where that is either true or false.
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

  /* -- 4c. the terminal renders what the agent says ---------------------- */
  // Terminal is still available on any pane, via the header toggle, and is
  // still the right tool when you need exactness. Ask for it explicitly now
  // that transcript is the default for agents.
  check(
    `the header toggle switches the agent pane to the terminal and it goes live`,
    await setView(page, SESSION, AGENT_PANE, 'terminal'),
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
  const shellOpen = await openPane(page, SESSION, SHELL_PANE)
  check(`the idle pane (${SHELL_PANE}) opens and goes live`, shellOpen)
  // A pane with no agent still DEFAULTS to the terminal (SPEC J2). Herdr
  // reports `agent_status: "unknown"` for every plain shell, so "does this
  // pane have an agent object" is the wrong question and once sent every
  // shell to the transcript.
  check(
    'a pane with no agent still defaults to the TERMINAL view',
    await page.locator('.term-host .xterm').isVisible().catch(() => false),
    'no stored preference for this pane',
  )
  // Resize shortly AFTER mount, which is what a real client does when it fits
  // itself to the box — and what made LIVE targets die silently before
  // 4e9aee3. Without this the idle soak below passes on a broken build.
  await sleep(800)
  await page.setViewportSize({ width: 1240, height: 860 })
  await sleep(800)
  await page.setViewportSize({ width: 1280, height: 900 })
  await sleep(2000)

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
  const mobileOpen = await openPane(page, SESSION, SHELL_PANE)
  check('the idle pane opens at 375px', mobileOpen)
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
  // The contrast, stated as an assertion rather than a claim: the terminal
  // view at 1280x900 DID resize this pane earlier in the run. That is exactly
  // the destruction AMENDMENTS 13 is about, and exactly what a phone must not
  // do to an agent's screen.
  check(
    'for contrast: the terminal view earlier in this run DID resize it',
    rowsBefore !== null && rowsBeforePhone !== null && rowsBeforePhone !== rowsBefore,
    `viewport_rows ${rowsBefore} (before any attach) -> ${rowsBeforePhone} (after the terminal view)`,
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
