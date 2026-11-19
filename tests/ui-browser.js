(async (page) => {
  'use strict';
  // Local Bridge fixture only: no real admin or model request is made.
  const assert = (value, message) => { if (!value) throw new Error(message); };
  const checks = [], errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const base = new URL(page.url()).origin;
  page.setDefaultTimeout(10000);
  await page.clock.install();
  await page.goto(`${base}/tests/ui-harness.html?run=${Date.now()}`);
  const ui = page.frameLocator('#plugin');
  const inner = page.frames().find((frame) => frame.url().includes('/ui/index.html'));
  const saved = () => page.evaluate(() => window.mock.config);
  const save = async (action) => {
    const count = await page.evaluate(() => window.mock.requests.filter((r) => r.type === 'config.save').length);
    await action();
    await page.waitForFunction((n) => window.mock.requests.filter((r) => r.type === 'config.save').length > n, count);
    await ui.locator('#automation-toggle:not(:disabled), #add-account-button:not(:disabled)').first().waitFor();
  };
  const poll = async () => {
    const count = await page.evaluate(() => window.mock.requests.filter((r) => r.type === 'plugin.status').length);
    await page.clock.runFor(10001);
    await page.waitForFunction((n) => window.mock.requests.filter((r) => r.type === 'plugin.status').length > n, count);
  };
  const field = (group, name) => ui.locator(`[data-rule-group="${group}"] [data-field="${name}"]`);
  const row = (id) => ui.locator(`[data-account-id="${id}"]`);
  await ui.locator('#bridge-state').getByText('已连接宿主', { exact: true }).waitFor();
  await ui.locator('.section-nav a[href=\"#view-monitor\"]').click();
  await page.clock.runFor(1500);
  await page.clock.runFor(10000);
  assert(await ui.locator('.section-nav a[href=\"#view-monitor\"]').evaluate((node) => node.classList.contains('active')), 'monitor nav remains active after smooth scrolling');
  await page.mouse.wheel(0, 1);
  await page.clock.runFor(100);
  assert(await ui.locator('.section-nav a[href=\"#view-monitor\"]').evaluate((node) => node.classList.contains('active')), 'monitor nav remains active after automated refresh');
  await ui.locator('.section-nav a[href=\"#view-groups\"]').click();
  await page.clock.runFor(1500);
  assert(await ui.locator('.section-nav a[href=\"#view-groups\"]').evaluate((node) => node.classList.contains('active')), 'management nav remains active after smooth scrolling');
  assert(await ui.locator('[data-rule-group]').count() === 4 && await ui.locator('#rule-group').count() === 0, 'four visible cards, no group dropdown');
  assert(await ui.locator('#view-groups input[type=number]').count() === 0, 'priorities edited only in rule cards');
  assert(await ui.locator('.group-section').count() === 5 && await ui.locator('#flow-steps .flow-step').count() === 4, 'temporary excluded from routing');
  assert(await ui.locator('#monitor-empty').isVisible() && await ui.locator('#monitor-group-list .monitor-group-card').count() === 0, 'monitor shows a single empty state when there are no accounts');
  const collapseButton = ui.locator('[data-group-id=excellent] .expand-button');
  const collapseBox = await collapseButton.boundingBox();
  assert(collapseBox && collapseBox.width >= 27 && collapseBox.height >= 27, 'collapse control has a stable hit target');
  assert(await collapseButton.innerText() === '', 'collapse control does not depend on a fallback glyph');
  assert(await collapseButton.getAttribute('aria-expanded') === 'true', 'collapse control exposes the expanded state');
  await collapseButton.click();
  assert(await ui.locator('[data-group-id=excellent] .group-content').isHidden(), 'collapse control hides group content');
  await poll();
  assert(await collapseButton.getAttribute('aria-expanded') === 'false' && await ui.locator('[data-group-id=excellent] .group-content').isHidden(), 'polling preserves the collapsed state');
  await collapseButton.click();
  assert(await collapseButton.getAttribute('aria-expanded') === 'true' && await ui.locator('[data-group-id=excellent] .group-content').isVisible(), 'collapse control restores group content');
  checks.push('four rule cards and fixed four-group routing');
  await ui.locator('#admin-url').fill('https://admin.example.invalid');
  await ui.locator('#admin-api-key').fill('ui-secret-canary');
  assert(await ui.locator('#collect-upstream, #upstream-rates-enabled').count() === 0, 'upstream snapshot switch is removed');
  for (const id of ['2081', '2082', '2083']) {
    await ui.locator('#new-account-id').fill(id);
    await save(() => ui.locator('#add-account-button').click());
  }
  assert((await saved()).account_settings.every((a) => a.group === 'temporary' && a.rate_mode === 'upstream'), 'new accounts default temporary/upstream');
  assert((await saved()).upstream_rates_enabled === true, 'legacy disabled snapshot config is normalized on save');
  await row('2082').locator('.row-edit').click();
  await ui.locator('#edit-group').selectOption('good');
  await save(() => ui.locator('#edit-save-button').click());
  await ui.locator('#account-dialog').waitFor({ state: 'hidden' });
  await row('2081').locator('.row-edit').click();
  await ui.locator('#edit-group').selectOption('excellent');
  await save(() => ui.locator('#edit-save-button').click());
  await ui.locator('#account-dialog').waitFor({ state: 'hidden' });
  await page.evaluate(() => {
    const a = { type: 'apikey', status: 'active', schedulable: true, read_ok: true, priority: 5, group: 'temporary', state: 'ready',
      rate_mode: 'upstream', fixed_rate: null, account_rate: 1, upstream_rate: null, requests: 20,
      cache_ratio: .78, complete: true, average_first_token_ms: 850, first_token_samples: 20 };
    window.mock.snapshot = { ...window.mock.snapshot, version: '0.1.0', mode: 'monitoring', accounts: [
      { ...a, id: 2081, name: '主力 API Key', platform: 'openai', upstream_rate: 1.3 },
      { ...a, id: 2082, name: '备用 API Key', platform: 'openai', group: 'good', state: 'unavailable', schedulable: false,
        effective_rate: 1,
        availability_reason: '账号过载至 2026-11-18T07:00:00Z', reason: '账号过载至 2026-11-18T07:00:00Z', requests: 0, cache_ratio: null, average_first_token_ms: null, first_token_samples: 19 },
      { ...a, id: 2083, name: 'Claude API Key', platform: 'anthropic' }],
      events: Array.from({ length: 21 }, (_, i) => ({ time: '2026-11-18T06:45:00Z', account_id: 2081,
        account_name: '主力 API Key', action: 'priority', message: `操作 ${i}` })) };
  });
  await poll();
  await ui.locator('#platform-select').selectOption('openai');
  await row('2081').getByText('主力 API Key', { exact: true }).waitFor();
  assert(await ui.locator('.account-row').count() === 2, 'management lists every configured account regardless of host availability');
  const monitor = ui.locator('#monitor-group-list');
  const monitorButton = (id) => monitor.locator(`[data-monitor-account-id="${id}"] .monitor-status`);
  const unavailable = (group) => monitor.locator(`[data-monitor-group="${group}"] .exception-area`);
  const schedule = (id) => monitor.locator(`[data-monitor-account-id="${id}"] .schedule-state`);
  assert((await monitor.locator('.monitor-table thead').first().innerText()).includes('调度情况'), 'monitor has an explicit scheduling column');
  assert(await ui.locator('#manual-poll').isDisabled() && await ui.locator('#monitor-countdown').innerText() === '未启用请求', 'manual collection is disabled while monitoring is off');
  assert(await monitor.locator('.monitor-group-card').count() === 2 && await monitor.locator('.exception-area').count() === 2, 'only populated call groups render, including an unavailable-only group');
  assert(await ui.locator('#overview-total').innerText() === '2' && await ui.locator('#monitor-empty').isHidden(), 'monitor counts the displayed groups');
  assert((await monitor.locator('[data-monitor-account-id="2082"] .rate-pair.default').innerText()).replace(/\s/g, '') === '上游不支持默认1.0×', 'missing upstream snapshot explicitly identifies the default fallback');
  assert(!(await monitor.innerText()).match(/850 ms|19 \/ 20|78%|20 \/ 20/), 'disabled monitor hides old usage');
  assert(await ui.locator('#monitor-poll-label').innerText() === '采样未启用', 'disabled monitor has an inactive sampling label');
  assert(await ui.locator('#view-groups .exception-area, #view-groups .account-overview').count() === 0, 'management contains only account configuration');
  assert(await ui.locator('#runtime-group-list, .runtime-groups').count() === 0 && !(await ui.locator('.runtime-section').innerText()).includes('调用组运行情况'), 'lower runtime section does not duplicate account groups');
  assert(await ui.locator('#runtime-platform').innerText() === 'OpenAI' && await ui.locator('#runtime-account-count').innerText() === '3', 'runtime summary retains the current platform and total managed account count');
  assert(await unavailable('good').locator('.exception-row').count() === 0, 'disabled monitoring does not expose stale runtime exceptions');
  await monitorButton('2082').click();
  assert(await ui.locator('#runtime-detail-dialog').isVisible() && await ui.locator('#account-dialog').isHidden(), 'runtime details use a separate dialog');
  assert((await ui.locator('#runtime-detail-host').innerText()).includes('未启用管理') && !(await ui.locator('#runtime-detail-reason').innerText()).includes('账号过载至'), 'disabled details do not retain the previous runtime diagnosis');
  assert(await ui.locator('#runtime-detail-cache').innerText() === '未采集' && await ui.locator('#runtime-detail-ttft').innerText() === '未采集', 'disabled details hide old metrics');
  await ui.locator('[data-close-runtime]').last().click();
  await page.evaluate(() => {
    Object.assign(window.mock.snapshot.accounts[1], { state: 'observing', schedulable: true, availability_reason: '', reason: '' });
  });
  await poll();
  await row('2082').waitFor();
  checks.push('ID addition, account names, platform filtering and TTFT');
  await row('2081').locator('.row-edit').click();
  await ui.locator('input[name=rate_mode][value=fixed]').check();
  await ui.locator('#edit-group').selectOption('general');
  await ui.locator('#edit-fixed-rate').fill('');
  await ui.locator('#edit-save-button').click();
  assert(await ui.locator('#account-dialog').isVisible() && await ui.locator('#edit-rate-error').isVisible(), 'blank fixed rate rejected');
  await ui.locator('#edit-fixed-rate').fill('1.25');
  await save(() => ui.locator('#edit-save-button').click());
  await ui.locator('#account-dialog').waitFor({ state: 'hidden' });
  await poll();
  assert((await saved()).account_settings.find((a) => a.account_id === 2081).fixed_rate === 1.25 && (await row('2081').innerText()).includes('1.25×'), 'exact fixed rate persists across old runtime snapshot');
  await row('2081').locator('.row-edit').click();
  assert(await ui.locator('#edit-fixed-rate').inputValue() === '1.25', 'edit round trip');
  assert(!(await ui.locator('input[name=rate_mode][value=upstream]').isDisabled()), 'upstream mode remains selectable after fixed mode');
  await ui.locator('input[name=rate_mode][value=upstream]').check();
  await save(() => ui.locator('#edit-save-button').click());
  await ui.locator('#account-dialog').waitFor({ state: 'hidden' });
  assert((await saved()).account_settings.find((a) => a.account_id === 2081).rate_mode === 'upstream', 'can switch fixed back to upstream');
  await row('2081').locator('.row-edit').click();
  assert(await ui.locator('input[name=rate_mode][value=upstream]').isChecked(), 'upstream mode round trip');
  await ui.locator('input[name=rate_mode][value=fixed]').check();
  assert(await ui.locator('#edit-fixed-rate').inputValue() === '1.25', 'fixed value retained when returning from upstream');
  await ui.locator('#edit-fixed-rate').fill('2');
  await page.evaluate(() => { window.mock.rejectTypes = ['config.save']; });
  await ui.locator('#edit-save-button').click();
  await ui.locator('#edit-rate-error').getByText(/保存未成功/).waitFor();
  assert(await ui.locator('#account-dialog').isVisible() && await ui.locator('#edit-fixed-rate').inputValue() === '2', 'failed save retains input');
  await page.evaluate(() => { window.mock.rejectTypes = []; });
  await ui.locator('[data-close-dialog]').last().click();
  checks.push('sandbox-safe fixed-rate save, blank validation and failure retention');
  await page.evaluate(() => { window.mock.rejectTypes = ['config.save']; });
  await ui.locator('#save-config').click();
  await ui.locator('#action-message').getByText(/permission denied/).waitFor();
  assert(!(await ui.locator('#action-message').innerText()).includes('ui-secret-canary') && !(await ui.locator('#action-message').innerText()).includes('宿主未完成请求'), 'specific host error is shown without leaking the key');
  await page.evaluate(() => { window.mock.rejectTypes = []; });
  checks.push('specific host errors are shown');
  await field('excellent', 'priority').fill('80');
  await field('excellent', 'cache-enabled').check();
  await field('excellent', 'cache-minimum').fill('55');
  await field('excellent', 'ttft-enabled').check();
  await field('excellent', 'ttft-maximum').fill('1200');
  await field('excellent', 'recovery-minutes').fill('2.5');
  assert(await field('excellent', 'elite-cache-minimum').inputValue() === '80' && await field('excellent', 'elite-ttft-maximum').inputValue() === '10000', 'independent exclusive thresholds default to 80 percent and 10000 ms');
  await field('excellent', 'elite-enabled').check();
  await field('excellent', 'elite-cache-minimum').fill('85');
  await field('excellent', 'elite-ttft-maximum').fill('8000');
  await field('good', 'priority').fill('1');
  await field('good', 'ttft-enabled').check();
  await field('good', 'ttft-maximum').fill('2500');
  await save(() => ui.locator('#save-rules').click());
  let c = await saved();
  assert(c.platform_group_rules.openai.excellent.min_cache_ratio === .55 && c.platform_group_rules.openai.excellent.cooldown_seconds === 150, 'percentage and seconds conversion');
  assert(c.platform_group_rules.openai.good.max_average_first_token_ms === 2500 && c.platform_group_priorities.openai.excellent === 80, 'independent group rules and priority');
  assert(c.platform_group_rules.openai.excellent.exclusive_min_cache_ratio === .85 && c.platform_group_rules.openai.excellent.exclusive_max_average_first_token_ms === 8000, 'exclusive thresholds save independently of ordinary thresholds');
  assert((await ui.locator('#route-order').innerText()).replace(/\s/g, '') === '2良好3一般', 'populated groups keep their fixed route order and ranks');
  assert((await ui.locator('[data-group-id=excellent] .group-settings-summary').innerText()).includes('1200 ms') && (await ui.locator('[data-group-id=excellent] .group-badge').innerText()).includes('80'), 'management settings summary');
  await ui.locator('#platform-select').selectOption('anthropic');
  assert(await ui.locator('#runtime-platform').innerText() === 'Anthropic', 'runtime summary follows platform selection without a group renderer');
  assert(await ui.locator('.account-row').count() === 1 && await field('excellent', 'priority').inputValue() === '6', 'platform defaults independent');
  assert(await field('excellent', 'elite-cache-minimum').inputValue() === '80' && await field('excellent', 'elite-ttft-maximum').inputValue() === '10000', 'exclusive thresholds are isolated by platform');
  assert(await monitor.locator('.monitor-group-card').count() === 1 && await ui.locator('#overview-total').innerText() === '1' &&
    await ui.locator('#monitor-empty').isHidden() && await ui.locator('#route-order').isHidden() && (await monitor.innerText()).includes('待定不调用'), 'temporary accounts show status in monitoring without entering routing');
  await field('excellent', 'priority').fill('60');
  await save(() => ui.locator('#save-rules').click());
  await ui.locator('#platform-select').selectOption('openai');
  assert(await field('excellent', 'priority').inputValue() === '80' && await field('excellent', 'ttft-maximum').inputValue() === '1200', 'platform round trip');
  assert(await field('excellent', 'elite-cache-minimum').inputValue() === '85' && await field('excellent', 'elite-ttft-maximum').inputValue() === '8000', 'exclusive thresholds round trip');
  checks.push('platform/group rule isolation, priorities and management summaries');
  const transfer = await inner.evaluateHandle(() => new DataTransfer());
  await row('2082').dispatchEvent('dragstart', { dataTransfer: transfer });
  await ui.locator('[data-group-id=reserve]').dispatchEvent('drop', { dataTransfer: transfer });
  await page.waitForFunction(() => window.mock.config.account_settings.find((a) => a.account_id === 2082)?.group === 'reserve');
  assert((await ui.locator('#automation-toggle').innerText()) === '启用', 'compact enabled automation label');
  assert(!(await ui.locator('#automation-toggle').isDisabled()), 'default 1.0 multiplier is available when upstream rate is missing');
  await row('2082').locator('.row-edit').click();
  await ui.locator('input[name=rate_mode][value=fixed]').check();
  await ui.locator('#edit-fixed-rate').fill('0');
  await save(() => ui.locator('#edit-save-button').click());
  await ui.locator('#account-dialog').waitFor({ state: 'hidden' });
  assert(!(await ui.locator('#automation-toggle').isDisabled()), 'explicit fixed zero accepted');
  assert((await row('2082').locator('.row-schedule').innerText()) === '停止调用', 'account schedule switch is visible');
  await save(() => row('2082').locator('.row-schedule').click());
  assert((await saved()).account_settings.find((a) => a.account_id === 2082).desired_schedulable === false, 'account schedule switch saves off');
  assert(await ui.locator('[data-group-id="reserve"] [data-account-id="2082"]').count() === 1, 'stop-call keeps the account in its group');
  assert((await row('2082').locator('.row-schedule').innerText()) === '恢复调用', 'account schedule switch shows stopped');
  await save(() => row('2082').locator('.row-schedule').click());
  assert((await row('2082').locator('.row-schedule').innerText()) === '停止调用', 'account schedule switch resumes group membership');
  await save(() => ui.locator('#automation-toggle').click());
  assert((await saved()).automation_enabled, 'automation saved');
  assert((await ui.locator('#automation-toggle').innerText()) === '关闭', 'compact disabled automation label');
  assert(await ui.locator('#save-rules').isDisabled() && await field('excellent', 'priority').isDisabled(), 'settings lock while automation is enabled');
  assert(await ui.locator('#new-account-id').isDisabled() && await row('2081').locator('.row-edit').isDisabled(), 'account edits lock while automation is enabled');
  assert(await ui.locator('#save-config').isDisabled() && await ui.locator('#admin-url').isDisabled(), 'connection settings lock while automation is enabled');
  assert(await row('2081').evaluate((node) => node.draggable === false), 'account group movement locks while automation is enabled');
  assert(await row('2081').locator('.row-schedule').isDisabled(), 'stop-call configuration locks while automation is enabled');
  assert(await field('excellent', 'elite-cache-minimum').isDisabled() && await field('excellent', 'elite-ttft-maximum').isDisabled(), 'exclusive settings lock while running');
  assert(!(await monitor.innerText()).includes('850 ms'), 'old monitoring snapshot stays hidden after enable');
  await poll();
  assert(!(await monitor.innerText()).includes('850 ms') && await ui.locator('#manual-poll').isDisabled(), 'old session snapshots cannot restore previous samples or enable collection controls');
  await page.evaluate(() => {
    Object.assign(window.mock.snapshot, { mode: 'automatic', sampling_active: true,
      sampling_since: new Date(Date.now()).toISOString(), polling: false, next_poll: new Date(Date.now() + 60000).toISOString() });
    Object.assign(window.mock.snapshot.accounts[0], { state: 'sampling', requests: 0, complete: false,
      cache_ratio: null, average_first_token_ms: null, first_token_samples: 0 });
  });
  await poll();
  await monitorButton('2081').getByText('等待采样 · 可调用').waitFor();
  assert((await monitor.locator('[data-monitor-account-id="2081"]').innerText()).includes('0 / 20 次'), 'new sampling window starts empty');
  assert(await schedule('2081').innerText() === '调度中' && await schedule('2081').evaluate((node) => getComputedStyle(node).color) === 'rgb(20, 119, 95)', 'current scheduling is shown in green');
  assert(!(await ui.locator('#manual-poll').isDisabled()), 'manual collection becomes available in the new session');
  const beforeCountdown = await ui.locator('#monitor-countdown').innerText();
  const beforeManual = await page.evaluate(() => ({ tests: window.mock.requests.filter((r) => r.type === 'config.test').length,
    saves: window.mock.requests.filter((r) => r.type === 'config.save').length,
    polls: window.mock.requests.filter((r) => r.type === 'plugin.status').length, session: window.mock.snapshot.sampling_since }));
  await page.clock.runFor(2100);
  const afterCountdown = await ui.locator('#monitor-countdown').innerText();
  const afterPolls = await page.evaluate(() => window.mock.requests.filter((r) => r.type === 'plugin.status').length);
  assert(afterCountdown !== beforeCountdown && afterPolls === beforeManual.polls, `countdown updates independently of status polling (${beforeCountdown} -> ${afterCountdown}, ${beforeManual.polls} -> ${afterPolls})`);
  await ui.locator('#manual-poll').click();
  await ui.locator('#monitor-countdown').getByText('正在请求…').waitFor();
  assert(await ui.locator('#manual-poll').isDisabled(), 'manual request stays disabled while collection is in flight');
  assert(await page.evaluate((before) => window.mock.requests.filter((r) => r.type === 'config.test').length === before.tests + 1 &&
    window.mock.requests.filter((r) => r.type === 'config.save').length === before.saves && window.mock.snapshot.sampling_since === before.session, beforeManual), 'manual request uses collection action without saving config or resetting session');
  await page.evaluate(() => Object.assign(window.mock.snapshot, { polling: false, last_poll: new Date().toISOString(), next_poll: new Date(Date.now() + 60000).toISOString() }));
  await poll();
  assert(!(await ui.locator('#manual-poll').isDisabled()) && (await ui.locator('#monitor-countdown').innerText()).startsWith('下次请求 '), 'completion restores manual request and countdown');
  checks.push('manual collection, session boundary and live countdown');
  await page.evaluate(() => {
    Object.assign(window.mock.snapshot.accounts[1], { state: 'unavailable', schedulable: false,
      availability_reason: '账号过载至 2026-11-19T11:05:00Z', reason: '账号过载至 2026-11-19T11:05:00Z' });
  });
  await poll();
  assert(await row('2082').count() === 1 && await row('2082').locator('.row-edit').isDisabled(), 'host-unavailable account remains listed and locked during monitoring');
  assert(await unavailable('reserve').locator('.exception-row[data-monitor-account-id="2082"]').count() === 1 && await monitorButton('2082').evaluate((node) => node.classList.contains('unavailable')), 'running host-unavailable state remains visible in account monitoring');
  assert(await schedule('2082').innerText() === '未调度' && await schedule('2082').evaluate((node) => getComputedStyle(node).color) === 'rgb(105, 118, 110)', 'closed scheduling is shown in gray in the unavailable list');
  assert((await unavailable('reserve').locator('thead').innerText()).includes('调度情况'), 'unavailable lists also have a scheduling column');
  await monitorButton('2082').click();
  assert((await ui.locator('#runtime-detail-host').innerText()).includes('账号过载至') && (await ui.locator('#runtime-detail-reason').innerText()).includes('账号过载至'), 'running details show the exact host reason');
  await ui.locator('[data-close-runtime]').last().click();
  await page.evaluate(() => {
    Object.assign(window.mock.snapshot.accounts[0], { state: 'ready', requests: 20, complete: true,
      cache_ratio: .78, average_first_token_ms: 850, first_token_samples: 20 });
    Object.assign(window.mock.snapshot.accounts[1], { state: 'group_wait', schedulable: false,
      reason: '', availability_reason: '', first_token_limit_reached: true });
  });
  await poll();
  await monitorButton('2081').getByText('可调用', { exact: true }).waitFor();
  assert((await monitor.innerText()).includes('850 ms') && await unavailable('reserve').locator('.exception-row.waiting[data-monitor-account-id="2082"]').count() === 1, 'active metrics render and group-wait accounts move to the unavailable area');
  await monitorButton('2082').click();
  assert((await ui.locator('#runtime-detail-ttft').innerText()).includes('19 / 20 · 已达 5000 条扫描上限'), 'unavailable account retains independent sampling details');
  await ui.locator('[data-close-runtime]').last().click();
  for (const state of ['exclusive_wait', 'manually_disabled', 'unknown', 'recovering']) {
    await page.evaluate((value) => Object.assign(window.mock.snapshot.accounts[1], { state: value, read_ok: value !== 'unknown' }), state);
    await poll();
    assert(await unavailable('reserve').locator('[data-monitor-account-id="2082"]').count() === 1 && await monitor.locator('[data-monitor-account-id="2082"]').count() === 1, `${state} appears once in its group unavailable area`);
    if (state === 'unknown') assert(await schedule('2082').innerText() === '待同步', 'an unread account does not claim scheduling is closed');
  }
  await monitorButton('2081').click();
  assert((await ui.locator('#runtime-detail-cache').innerText()).includes('78.0%'), 'monitor opens actual sampling details');
  await ui.locator('[data-close-runtime]').last().click();
  await page.evaluate(() => {
    Object.assign(window.mock.snapshot.accounts[0], { state: 'paused', schedulable: false, reason: '缓存命中低于下限', recovery_at: '2026-11-19T11:05:00Z' });
    Object.assign(window.mock.snapshot.accounts[1], { state: 'manual_change', reason: '运行监控后检测到宿主将账号设为不可调度；已进入手动操作组，保持调度关闭' });
  });
  await poll();
  await unavailable('general').locator('.exception-row.recoverable[data-monitor-account-id="2081"]').waitFor();
  await unavailable('reserve').locator('.exception-row.manual[data-monitor-account-id="2082"]').waitFor();
  const yellow = 'rgb(150, 101, 21)';
  assert(await unavailable('reserve').locator('.exception-row.manual').evaluate((node) => getComputedStyle(node).backgroundColor) === 'rgb(255, 251, 235)' &&
    await monitorButton('2082').evaluate((node) => getComputedStyle(node).color) === yellow, 'manual area and account status use yellow');
  assert(await row('2081').count() === 1 && await row('2082').count() === 1, 'rule and manual exceptions remain listed in account management');
  assert(await monitorButton('2082').innerText() === '用户手动操作' && await monitorButton('2081').innerText() === '可自动恢复' &&
    !(await unavailable('reserve').innerText()).includes('运行监控后检测到宿主'), 'unavailable rows use recovery/manual status labels without repeating the full explanation');
  assert(await unavailable('reserve').locator('.monitor-identity').innerText() === '备用 API Key\nID 2082' &&
    (await unavailable('reserve').locator('thead th').allTextContents()).join('|') === '账号|采用倍率|调度情况|缓存比例|采样|平均首字时间|运行状态', 'unavailable rows reuse the complete normal-account format');
  assert(await row('2081').locator('.row-edit').isDisabled() && await row('2082').locator('.row-schedule').isDisabled(), 'exception account settings stay locked while running');
  await monitorButton('2081').click();
  assert((await ui.locator('#runtime-detail-recovery').innerText()).includes('开始重新采样'), 'recoverable exception shows recovery time');
  await ui.locator('[data-close-runtime]').last().click();
  await monitorButton('2082').click();
  assert((await ui.locator('#runtime-detail-recovery').innerText()).includes('当前调度：关闭') &&
    await ui.locator('#runtime-detail-status').evaluate((node) => getComputedStyle(node).color) === yellow, 'manual details show the actual closed scheduling state in yellow');
  assert((await ui.locator('#runtime-detail-reason').innerText()).includes('运行监控后检测到宿主将账号设为不可调度'), 'full manual reason remains available in the separate detail window');
  await ui.locator('[data-close-runtime]').last().click();
  await page.evaluate(() => { window.mock.snapshot.accounts[1].schedulable = true; });
  await poll();
  assert(await schedule('2082').innerText() === '调度中' && await schedule('2082').evaluate((node) => getComputedStyle(node).color) === 'rgb(20, 119, 95)', 'manual scheduling is reported from the host even when the account is outside automatic management');
  await monitorButton('2082').click();
  assert((await ui.locator('#runtime-detail-status').innerText()).includes('调度开启') && (await ui.locator('#runtime-detail-recovery').innerText()).includes('当前调度：开启'), 'manual details distinguish open scheduling');
  await ui.locator('[data-close-runtime]').last().click();
  await page.evaluate(() => Object.assign(window.mock.snapshot.accounts[2], { platform: 'openai', state: 'manual_change', schedulable: true, reason: '人工开启临时账号调度' }));
  await poll();
  assert(await unavailable('temporary').locator('.exception-row.manual[data-monitor-account-id="2083"]').count() === 1 && !(await ui.locator('#route-order').innerText()).includes('临时'), 'manual temporary account is visible but never enters routing');
  await page.evaluate(() => Object.assign(window.mock.snapshot.accounts[2], { platform: 'anthropic', state: 'temporary', schedulable: false, reason: '临时组不参与调用，优先级固定为 99' }));
  await poll();
  await ui.locator('#platform-select').selectOption('anthropic');
  assert(await unavailable('temporary').locator('.exception-row.inactive[data-monitor-account-id="2083"]').count() === 1 && await schedule('2083').innerText() === '未调度' &&
    (await monitor.locator('[data-monitor-group="temporary"] header').innerText()).includes('1 个账号 · 不参与自动调用') && await ui.locator('#route-order').isHidden(), 'ordinary temporary account remains visible as not participating while running');
  await ui.locator('#platform-select').selectOption('openai');
  for (const width of [1180, 780, 390, 320]) {
    await page.setViewportSize({ width, height: 871 });
    assert(await inner.evaluate(() => [...document.querySelectorAll('.exception-row')].every((row) => {
      const box = row.getBoundingClientRect();
      return [...row.querySelectorAll('.monitor-status, .rate-pair, .schedule-state')].every((node) => {
        const child = node.getBoundingClientRect();
        return child.left >= box.left - 1 && child.right <= box.right + 1;
      });
    })), `compact exception columns fit inside the list at ${width}`);
    assert(await inner.evaluate(() => {
      const headers = [...document.querySelectorAll('#monitor-group-list .monitor-table thead tr')].map((row) => [...row.children].map((cell) => cell.getBoundingClientRect().left));
      return headers.length > 1 && headers.every((cells) => cells.length === 7 && cells.every((left, index) => Math.abs(left - headers[0][index]) < 1));
    }), `normal and unavailable lists use identical column positions at ${width}`);
  }
  await page.setViewportSize({ width: 1180, height: 900 });
  checks.push('compact monitoring lists and host scheduling colors');
  checks.push('unavailable categories and yellow manual group with scheduling state');
  await ui.locator('#event-next').click();
  await poll();
  assert(await ui.locator('#event-page-label').innerText() === '第 2 / 3 页', 'log polling preserves page');
  assert(!(await ui.locator('#event-clear').isDisabled()), 'clear records is available when records exist');
  await ui.locator('#event-clear').click();
  await ui.locator('#clear-events-confirm').click();
  assert(await ui.locator('#event-page-label').innerText() === '第 1 / 1 页', 'event pagination resets after clear');
  checks.push('clear operation records');
  assert(await row('2082').locator('.row-remove').isDisabled(), 'account removal locks while automation is enabled');
  await save(() => ui.locator('#automation-toggle').click());
  assert(!(await saved()).automation_enabled, 'automation can be closed to unlock settings');
  assert(!(await monitor.innerText()).match(/850 ms|78\.0%|20 \/ 20/), 'stop hides usage even if the host keeps returning the active snapshot');
  assert(await monitor.locator('.exception-row').count() === 0, 'stop clears runtime exceptions even while the previous snapshot is returned');
  assert(await row('2081').count() === 1 && await row('2082').count() === 1 && !(await row('2081').locator('.row-edit').isDisabled()) && !(await row('2082').locator('.row-schedule').isDisabled()), 'all managed accounts remain editable after stop');
  await monitorButton('2081').click();
  assert(await ui.locator('#runtime-detail-cache').innerText() === '未采集' && await ui.locator('#runtime-detail-ttft').innerText() === '未采集', 'stop also hides metrics in reopened details');
  assert((await ui.locator('#runtime-detail-status').innerText()) === '未启用管理' && await ui.locator('#runtime-detail-recovery').evaluate((node) => node.parentElement.hidden), 'stopped details clear runtime state and recovery countdown');
  await ui.locator('[data-close-runtime]').last().click();
  assert(!(await ui.locator('#save-rules').isDisabled()) && !(await ui.locator('#admin-url').isDisabled()), 'settings unlock after automation is disabled');
  await save(() => ui.locator('#automation-toggle').click());
  await poll();
  assert(await monitor.locator('.exception-row.manual, .exception-row.recoverable').count() === 0 && !(await monitor.innerText()).includes('缓存命中低于下限'), 're-enable immediately clears old manual and paused states despite old-session snapshot');
  await page.evaluate(() => {
    Object.assign(window.mock.snapshot, { sampling_since: new Date().toISOString(), polling: false });
    for (const account of window.mock.snapshot.accounts) Object.assign(account, { state: 'sampling', schedulable: account.id === 2081,
      reason: '', availability_reason: '', recovery_at: null, requests: 0, cache_ratio: null, complete: false,
      average_first_token_ms: null, first_token_samples: 0 });
  });
  await poll();
  await monitorButton('2081').getByText('等待采样 · 可调用').waitFor();
  assert(!(await monitor.innerText()).includes('缓存命中低于下限') && await monitor.locator('.exception-row.manual, .exception-row.recoverable').count() === 0, 'new session begins with fresh sampling state');
  await save(() => ui.locator('#automation-toggle').click());
  assert(await ui.locator('#manual-poll').isDisabled(), 'manual collection disables again on stop');
  checks.push('explicit off/on resets every previous monitoring state');
  await row('2082').locator('.row-remove').click();
  await save(() => ui.locator('#delete-account-confirm').click());
  await ui.locator('#delete-account-dialog').waitFor({ state: 'hidden' });
  await poll();
  assert(await row('2082').count() === 0 && !(await saved()).account_ids.includes(2082), 'deleted ID cannot return from old snapshot');
  await row('2081').locator('.row-edit').click();
  await ui.locator('input[name=rate_mode][value=upstream]').check();
  await save(() => ui.locator('#edit-save-button').click());
  await ui.locator('#account-dialog').waitFor({ state: 'hidden' });
  assert((await row('2081').locator('.rate-pair.upstream').innerText()).replace(/\s/g, '') === '上游1.3×', 'upstream selection adopts the snapshot even with a saved fixed rate');
  await page.evaluate(() => Object.assign(window.mock.snapshot.accounts[0], { upstream_rate: null, rate_mode: 'default', effective_rate: 1 }));
  await poll();
  assert((await row('2081').locator('.rate-pair.default').innerText()).replace(/\s/g, '') === '上游不支持默认1.0×', 'unsupported upstream uses default 1 rather than the saved fixed rate');
  assert(!(await ui.locator('body').innerText()).includes('ui-secret-canary'), 'secret not rendered');
  checks.push('account dragging, default-rate fallback, explicit zero, logs and deletion');
  for (const width of [1440, 1180, 1050, 980, 800, 781, 780, 766, 480, 390, 320]) {
    await page.setViewportSize({ width, height: 871 });
    assert(await inner.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `no page overflow at ${width}`);
    assert(await inner.evaluate(() => [...document.querySelectorAll('.group-section')].every((group) => {
      const box = group.getBoundingClientRect();
      return [...group.querySelector('.group-header').children].every((node) => {
        const child = node.getBoundingClientRect();
        return child.left >= box.left && child.right <= box.right;
      });
    })), `group header and collapse control stay inside their own card at ${width}`);
    assert(await inner.evaluate(() => [...document.querySelectorAll('.account-row')].every((row) => {
      const box = row.getBoundingClientRect();
      return [...row.querySelectorAll('.account-actions, .row-schedule, .row-edit, .row-remove')].every((node) => {
        const child = node.getBoundingClientRect();
        return child.left >= box.left - 1 && child.right <= box.right + 1;
      });
    })), `account actions stay inside row at ${width}`);
  }
  assert(await inner.evaluate(() => document.querySelector('#view-monitor').getBoundingClientRect().top < document.querySelector('#view-groups').getBoundingClientRect().top), 'monitor first');
  assert(errors.length === 0, errors.join('; '));
  await page.setViewportSize({ width: 1180, height: 900 });
  await ui.locator('#view-rules').scrollIntoViewIfNeeded();
  checks.push('responsive layout and no JS errors');
  return { passed: checks };
})
