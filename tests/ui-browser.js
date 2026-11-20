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
  const inner = page.frames().find((frame) => frame.url().includes('/ui/index.html'));
  await ui.locator('#top-version').getByText('v0.1.1-test', { exact: true }).waitFor();
  assert((await ui.locator('#runtime-version').innerText()) === '0.1.1-test', 'both version labels read the runtime snapshot');
  assert(await ui.locator('.section-nav > #automation-toggle:last-child').innerText() === '启动监控', 'monitor control is the last item on the navigation bar');
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
  assert(await ui.locator('[data-rule-group]').count() === 5 && await ui.locator('#rule-group').count() === 0, 'four rule cards plus always-on, no group dropdown');
  assert((await ui.locator('[data-rule-group=always_on] input').evaluateAll((inputs) => inputs.map((input) => input.dataset.field).sort())).join('|') === 'min-input-tokens|priority|retry-seconds', 'always-on offers priority, retry base and sampling threshold but no quality overrides');
  assert(await field('always_on', 'priority').inputValue() === '5' && await field('always_on', 'retry-seconds').inputValue() === '300', 'always-on uses priority 5 and a 300s default');
  const tokenFields = ui.locator('[data-rule-group] [data-field="min-input-tokens"]');
  assert(await tokenFields.count() === 5 && (await tokenFields.evaluateAll((inputs) => inputs.map((input) => input.value))).every((value) => value === '40000'), 'all five call groups default to a 40,000-input-token sampling threshold');
  assert(await field('excellent', 'trigger-samples').inputValue() === '2' && await field('excellent', 'recovery-samples').inputValue() === '2' &&
    await field('excellent', 'trigger-samples').getAttribute('min') === '2' && await field('excellent', 'sample-count').count() === 0, 'legacy one-window rules are floored to two, sample size is not editable');
  assert(await ui.locator('#view-groups input[type=number]').count() === 0, 'priorities edited only in rule cards');
  assert(await ui.locator('.group-section').count() === 6 && await ui.locator('#flow-steps .flow-step').count() === 5 &&
    (await ui.locator('#flow-steps .parallel').innerText()).replace(/\s/g, '') === '常开·并行P5', 'always-on has a parallel path; temporary excluded from routing');
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
  checks.push('live version, monitor CTA, five rule cards and parallel always-on routing');
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
      rate_mode: 'upstream', fixed_rate: null, account_rate: 1, upstream_rate: null, requests: 15,
      cache_ratio: .78, complete: true, average_first_token_ms: 850, first_token_samples: 15 };
    window.mock.snapshot = { ...window.mock.snapshot, version: '0.1.1-Beta1', mode: 'monitoring', accounts: [
      { ...a, id: 2081, name: '主力 API Key', platform: 'openai', upstream_rate: 1.3 },
      { ...a, id: 2082, name: '备用 API Key', platform: 'openai', group: 'good', state: 'unavailable', schedulable: false,
        effective_rate: 1,
        availability_reason: '账号过载至 2026-11-18T07:00:00Z', reason: '账号过载至 2026-11-18T07:00:00Z', requests: 0, cache_ratio: null, average_first_token_ms: null, first_token_samples: 14 },
      { ...a, id: 2083, name: 'Claude API Key', platform: 'anthropic' }],
      events: Array.from({ length: 21 }, (_, i) => ({ time: '2026-11-18T06:45:00Z', account_id: 2081,
        account_name: '主力 API Key', action: 'priority', message: `操作 ${i}` })) };
  });
  await poll();
  await ui.locator('#top-version').getByText('v0.1.1-Beta1', { exact: true }).waitFor();
  await ui.locator('#platform-select').selectOption('openai');
  await row('2081').getByText('主力 API Key', { exact: true }).waitFor();
  assert(await ui.locator('.account-row').count() === 2, 'management lists every configured account regardless of host availability');
  const monitor = ui.locator('#monitor-group-list');
  const monitorRow = (id) => monitor.locator(`[data-monitor-account-id="${id}"]`);
  const historyRows = (id) => monitor.locator(`tr.monitor-history-row[data-history-account-id="${id}"]`);
  const monitorButton = (id) => monitorRow(id).locator('.monitor-status');
  const unavailable = (group) => monitor.locator(`[data-monitor-group="${group}"] .exception-area`);
  const schedule = (id) => monitor.locator(`[data-monitor-account-id="${id}"] .schedule-state`);
  assert((await monitor.locator('.monitor-table thead').first().innerText()).includes('调度情况'), 'monitor has an explicit scheduling column');
  assert(await ui.locator('#manual-poll').isDisabled() && await ui.locator('#monitor-countdown').innerText() === '未启用请求', 'manual collection is disabled while monitoring is off');
  assert(await monitor.locator('.monitor-group-card').count() === 2 && await monitor.locator('.exception-area').count() === 2, 'only populated call groups render, including an unavailable-only group');
  assert(await ui.locator('#overview-total').innerText() === '2' && await ui.locator('#monitor-empty').isHidden(), 'monitor counts the displayed groups');
  assert((await monitor.locator('[data-monitor-account-id="2082"] .rate-pair.default').innerText()).replace(/\s/g, '') === '上游不支持默认1.0×', 'missing upstream snapshot explicitly identifies the default fallback');
  assert(!(await monitor.innerText()).match(/850ms|14 \/ 15|78%|15 \/ 15/), 'disabled monitor hides old usage');
  assert(await ui.locator('#monitor-poll-label').innerText() === '采样未启用', 'disabled monitor has an inactive sampling label');
  assert(await ui.locator('#view-groups .exception-area, #view-groups .account-overview').count() === 0, 'management contains only account configuration');
  assert(await ui.locator('#runtime-group-list, .runtime-groups').count() === 0 && !(await ui.locator('.runtime-section').innerText()).includes('调用组运行情况'), 'lower runtime section does not duplicate account groups');
  assert(await ui.locator('#runtime-platform').innerText() === 'OpenAI' && await ui.locator('#runtime-account-count').innerText() === '3', 'runtime summary retains the current platform and total managed account count');
  assert(await unavailable('good').locator('.exception-row').count() === 0, 'disabled monitoring does not expose stale runtime exceptions');
  await monitorButton('2082').click();
  assert(await ui.locator('#runtime-detail-dialog').isVisible() && await ui.locator('#account-dialog').isHidden(), 'runtime details use a separate dialog');
  assert((await ui.locator('#runtime-detail-host').innerText()).includes('未启用管理') && !(await ui.locator('#runtime-detail-reason').innerText()).includes('账号过载至'), 'disabled details do not retain the previous runtime diagnosis');
  assert(await ui.locator('#runtime-detail-cache').innerText() === '未采集' && await ui.locator('#runtime-detail-ttft').innerText() === '未采集', 'disabled details hide old metrics');
  assert(await ui.locator('#runtime-account-events > li').count() === 0 &&
    await ui.locator('#runtime-account-events-empty').isVisible(), 'an account with no operation records shows an explicit empty state');
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
  await field('excellent', 'retry-seconds').fill('151');
  await field('excellent', 'confirmation-seconds').fill('180');
  await field('excellent', 'recovery-seconds').fill('330');
  await field('always_on', 'retry-seconds').fill('320');
  const excellentThreshold = field('excellent', 'min-input-tokens');
  for (const invalid of ['0', '40000.5']) {
    await excellentThreshold.fill(invalid);
    assert(!await excellentThreshold.evaluate((input) => input.checkValidity()), `rejects nonpositive or fractional token threshold ${invalid}`);
  }
  await excellentThreshold.fill('56000');
  await field('always_on', 'min-input-tokens').fill('48000');
  assert(await field('excellent', 'elite-cache-minimum').inputValue() === '80' && await field('excellent', 'elite-ttft-maximum').inputValue() === '10000', 'independent exclusive thresholds default to 80 percent and 10000 ms');
  await field('excellent', 'elite-enabled').check();
  await field('excellent', 'elite-cache-minimum').fill('85');
  await field('excellent', 'elite-ttft-maximum').fill('8000');
  await field('good', 'priority').fill('1');
  await field('good', 'ttft-enabled').check();
  await field('good', 'ttft-maximum').fill('2500');
  assert(await field('excellent', 'cache-minimum').evaluate((node) => node.classList.contains('rule-customized') && getComputedStyle(node).color === 'rgb(20, 119, 95)') &&
    await excellentThreshold.evaluate((node) => node.classList.contains('rule-customized') && getComputedStyle(node).color === 'rgb(20, 119, 95)') &&
    !await field('excellent', 'confirmation-seconds').evaluate((node) => node.classList.contains('rule-customized')), 'changed parameters turn green while default values retain their normal style');
  await save(() => ui.locator('#save-rules').click());
  let c = await saved();
  assert(c.platform_group_rules.openai.excellent.min_requests === 15 && c.platform_group_rules.openai.excellent.min_cache_ratio === .55 && c.platform_group_rules.openai.excellent.cooldown_seconds === 151 &&
    c.platform_group_rules.openai.excellent.recovery_seconds === 330 && c.platform_group_rules.openai.always_on.cooldown_seconds === 320, 'quality rules and always-on retry seconds saved exactly');
  assert(c.platform_group_rules.openai.excellent.min_input_tokens === 56000 && c.platform_group_rules.openai.always_on.min_input_tokens === 48000, 'sampling thresholds are saved as exact token counts for both group types');
  assert(c.platform_group_rules.openai.good.max_average_first_token_ms === 2500 && c.platform_group_priorities.openai.excellent === 80, 'independent group rules and priority');
  assert(c.platform_group_rules.openai.excellent.exclusive_min_cache_ratio === .85 && c.platform_group_rules.openai.excellent.exclusive_max_average_first_token_ms === 8000, 'exclusive thresholds save independently of ordinary thresholds');
  assert((await ui.locator('#route-order').innerText()).replace(/\s/g, '') === '2良好3一般', 'populated groups keep their fixed route order and ranks');
  assert((await ui.locator('[data-group-id=excellent] .group-settings-summary').innerText()).includes('1200 ms') && (await ui.locator('[data-group-id=excellent] .group-badge').innerText()).includes('80'), 'management settings summary');
  await ui.locator('#platform-select').selectOption('anthropic');
  assert(await excellentThreshold.inputValue() === '40000' && await field('always_on', 'min-input-tokens').inputValue() === '40000', 'OpenAI threshold changes do not affect the Anthropic platform');
  assert(await ui.locator('#runtime-platform').innerText() === 'Anthropic', 'runtime summary follows platform selection without a group renderer');
  assert(await ui.locator('.account-row').count() === 1 && await field('excellent', 'priority').inputValue() === '6', 'platform defaults independent');
  assert(await field('excellent', 'elite-cache-minimum').inputValue() === '80' && await field('excellent', 'elite-ttft-maximum').inputValue() === '10000', 'exclusive thresholds are isolated by platform');
  assert(await monitor.locator('.monitor-group-card').count() === 1 && await ui.locator('#overview-total').innerText() === '1' &&
    await ui.locator('#monitor-empty').isHidden() && await ui.locator('#route-order').isHidden() && (await monitor.innerText()).includes('待定不调用'), 'temporary accounts show status in monitoring without entering routing');
  await field('excellent', 'priority').fill('60');
  await excellentThreshold.fill('52000');
  await save(() => ui.locator('#save-rules').click());
  await ui.locator('#platform-select').selectOption('openai');
  assert(await field('excellent', 'priority').inputValue() === '80' && await field('excellent', 'ttft-maximum').inputValue() === '1200', 'platform round trip');
  assert(await excellentThreshold.inputValue() === '56000' && await field('always_on', 'min-input-tokens').inputValue() === '48000', 'thresholds survive platform round trip independently');
  assert(await field('excellent', 'priority').evaluate((node) => node.classList.contains('rule-customized')), 'saved non-default parameters stay highlighted after a platform round trip');
  assert(await field('excellent', 'elite-cache-minimum').inputValue() === '85' && await field('excellent', 'elite-ttft-maximum').inputValue() === '8000', 'exclusive thresholds round trip');
  checks.push('platform/group rule isolation, priorities and management summaries');
  const transfer = await inner.evaluateHandle(() => new DataTransfer());
  await row('2082').dispatchEvent('dragstart', { dataTransfer: transfer });
  await ui.locator('[data-group-id=reserve]').dispatchEvent('drop', { dataTransfer: transfer });
  await page.waitForFunction(() => window.mock.config.account_settings.find((a) => a.account_id === 2082)?.group === 'reserve');
  assert((await ui.locator('#automation-toggle').innerText()) === '启动监控', 'explicit start-monitor label');
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
  await ui.locator('#new-account-id').fill('2084');
  await save(() => ui.locator('#add-account-button').click());
  await page.evaluate(() => {
    window.mock.snapshot.accounts.push({ ...window.mock.snapshot.accounts[0], id: 2084, name: '常开 API Key', priority: 5 });
  });
  await poll();
  await row('2084').getByText('常开 API Key', { exact: true }).waitFor();
  await row('2084').locator('.row-edit').click();
  await ui.locator('#edit-group').selectOption('always_on');
  await save(() => ui.locator('#edit-save-button').click());
  await ui.locator('#account-dialog').waitFor({ state: 'hidden' });
  assert((await ui.locator('#route-order .parallel').innerText()) === '常开 · 并行', 'always-on account creates a parallel route');
  await save(() => ui.locator('#automation-toggle').click());
  assert((await saved()).automation_enabled, 'automation saved');
  assert((await ui.locator('#automation-toggle').innerText()) === '停止监控', 'explicit stop-monitor label');
  assert(await ui.locator('#save-rules').isDisabled() && await ui.locator('#reset-rules').isDisabled() && await field('excellent', 'priority').isDisabled(), 'save, restore defaults and parameters lock while automation is enabled');
  assert((await tokenFields.evaluateAll((inputs) => inputs.map((input) => input.disabled))).every(Boolean), 'all token thresholds lock while monitoring runs');
  assert(await ui.locator('#new-account-id').isDisabled() && await row('2081').locator('.row-edit').isDisabled(), 'account edits lock while automation is enabled');
  assert(await ui.locator('#save-config').isDisabled() && await ui.locator('#admin-url').isDisabled(), 'connection settings lock while automation is enabled');
  assert(await row('2081').evaluate((node) => node.draggable === false), 'account group movement locks while automation is enabled');
  assert(await row('2081').locator('.row-schedule').isDisabled(), 'stop-call configuration locks while automation is enabled');
  assert(await field('excellent', 'elite-cache-minimum').isDisabled() && await field('excellent', 'elite-ttft-maximum').isDisabled(), 'exclusive settings lock while running');
  assert(!(await monitor.innerText()).includes('850ms'), 'old monitoring snapshot stays hidden after enable');
  await poll();
  assert(!(await monitor.innerText()).includes('850ms') && await ui.locator('#manual-poll').isDisabled(), 'old session snapshots cannot restore previous samples or enable collection controls');
  await page.evaluate(() => {
    Object.assign(window.mock.snapshot, { mode: 'automatic', sampling_active: true,
      sampling_since: new Date(Date.now()).toISOString(), polling: false, next_poll: new Date(Date.now() + 60000).toISOString() });
    Object.assign(window.mock.snapshot.accounts[0], { state: 'sampling', requests: 0, complete: false,
      cache_ratio: null, average_first_token_ms: null, first_token_samples: 0 });
  });
  await poll();
  await monitorButton('2081').getByText('等待采样 · 可调用').waitFor();
  const newSamplingCell = await monitorRow('2081').locator('.monitor-window-sample').innerText();
  assert(newSamplingCell === '等待采样' && !newSamplingCell.includes('0/15'), 'no discovered bucket yet does not imply a known empty window');
  assert(await schedule('2084').innerText() === '调度中' && await monitorButton('2084').innerText() === '可调用', 'always-on and ordinary winner run together');
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
  await page.waitForFunction((count) => window.mock.requests.filter((r) => r.type === 'config.test').length > count, beforeManual.tests);
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
    Object.assign(window.mock.snapshot.accounts[0], { state: 'ready', requests: 15, complete: true,
      cache_ratio: .78, average_first_token_ms: 850, first_token_samples: 15 });
    Object.assign(window.mock.snapshot.accounts[1], { state: 'group_wait', schedulable: false,
      reason: '', availability_reason: '', first_token_limit_reached: true });
  });
  await poll();
  await monitorButton('2081').getByText('可调用', { exact: true }).waitFor();
  assert((await monitor.innerText()).includes('850ms') && await unavailable('reserve').locator('.exception-row.waiting[data-monitor-account-id="2082"]').count() === 1, 'active metrics render and group-wait accounts move to the unavailable area');
  await monitorButton('2082').click();
  assert((await ui.locator('#runtime-detail-ttft').innerText()).includes('14 / 15 · 已达 5000 条扫描上限'), 'unavailable account retains independent sampling details');
  await ui.locator('[data-close-runtime]').last().click();
  for (const state of ['exclusive_wait', 'manually_disabled', 'unknown', 'recovering']) {
    await page.evaluate((value) => Object.assign(window.mock.snapshot.accounts[1], { state: value, read_ok: value !== 'unknown',
      error: value === 'unknown' ? '管理 API 权限不足：当前密钥未开放此操作' : '' }), state);
    await poll();
    assert(await unavailable('reserve').locator('[data-monitor-account-id="2082"]').count() === 1 && await monitor.locator('[data-monitor-account-id="2082"]').count() === 1, `${state} appears once in its group unavailable area`);
    if (state === 'unknown') {
      assert(await schedule('2082').innerText() === '待同步', 'an unread account does not claim scheduling is closed');
      await monitorButton('2082').click();
      assert((await ui.locator('#runtime-detail-error').innerText()).includes('管理 API 权限不足') &&
        await ui.locator('#runtime-detail-error-requests').innerText() === '宿主暂不提供，不能统计502',
        'unread account exposes management permission diagnostics without inferring model errors');
      await ui.locator('[data-close-runtime]').last().click();
    }
  }
  await monitorButton('2081').click();
  assert((await ui.locator('#runtime-detail-cache').innerText()).includes('78.0%'), 'monitor opens actual sampling details');
  await ui.locator('[data-close-runtime]').last().click();
  await page.evaluate(() => {
    const since = new Date().toISOString();
    const previous = new Date(Date.now() - 200000).toISOString();
    const at = (secs) => new Date(Date.now() + secs * 1000).toISOString();
    Object.assign(window.mock.snapshot.accounts[0], {
      state: 'paused', schedulable: false, reason: '缓存命中低于下限',
      error: '管理 API 连接失败或超时：ui-secret-canary <em>transport</em>', priority: 8,
      requests: 15, complete: true, cache_ratio: .58, average_first_token_ms: 1122, first_token_samples: 15,
      min_input_tokens: 40000, excluded_small_requests: 9, excluded_unknown_requests: 2,
      status_since: since, failure_kind: 'quality', failure_count: 5, temporary_failures: 1,
      recovery_attempts: 4, recovery_failures: 3, last_failure_at: since, last_recovery_attempt_at: previous,
      next_retry_at: new Date(Date.now() + 600000).toISOString(), degradation_level: 2,
      quality_bad_windows: 2, quality_good_windows: 0, quality_scan_limit_reached: true, error_metrics_available: false,
      quality_sampling: { window_known: true, requests: 6, first_token_samples: 6,
        cache_ratio: null, average_first_token_ms: null, bad_windows: 2, good_windows: 0,
        min_input_tokens: 40000, excluded_small_requests: 2, excluded_unknown_requests: 1,
        last_evaluated_at: since, reason: '缓存命中低于下限',
        models: [
          { model: 'gpt-fixture', request_type: 'stream', requests: 4, cache_ratio: .8, average_first_token_ms: 810, first_token_samples: 4 },
          { model: 'other-model-fixture', request_type: 'sync', requests: 2, cache_ratio: .6, average_first_token_ms: 840, first_token_samples: 2 },
        ],
        history: [
          window.qualityWindow(at(-60), { cache_ratio: .451, average_first_token_ms: 902, excluded_small_requests: 4,
            excluded_unknown_requests: 1, result: 'abnormal', reason: '账号窗口整体缓存过低', models: [
              { model: 'gpt-fixture', request_type: 'stream', requests: 10, cache_ratio: .4, average_first_token_ms: 900, first_token_samples: 10 },
              { model: 'other-model-fixture', request_type: 'sync', requests: 5, cache_ratio: .7, average_first_token_ms: 906, first_token_samples: 5 },
            ] }),
          window.qualityWindow(at(-280), { cache_ratio: .733, average_first_token_ms: null,
            first_token_samples: 4, result: 'unknown', reason: '账号窗口首字样本不足', models: [
              { model: '<img src=x onerror=alert(1)> ui-secret-canary', request_type: 'stream', requests: 15,
                cache_ratio: .733, average_first_token_ms: null, first_token_samples: 4 },
            ] }),
          window.qualityWindow(at(-170), { cache_ratio: .922, average_first_token_ms: 812, excluded_small_requests: 6, recorded_at: at(-8), models: [
            { model: 'other-model-fixture', request_type: 'sync', requests: 15, cache_ratio: .922, average_first_token_ms: 812, first_token_samples: 15 },
          ] }),
          window.qualityWindow(at(-390), { cache_ratio: .614, average_first_token_ms: 617 }),
        ] },
    });
    Object.assign(window.mock.snapshot.accounts[1], { state: 'manual_change', reason: '运行监控后检测到宿主将账号设为不可调度；已进入手动操作组，保持调度关闭' });
    Object.assign(window.mock.snapshot.accounts[3], { min_input_tokens: 48000, requests: 15, complete: true,
      cache_ratio: .6, average_first_token_ms: 950, first_token_samples: 15, excluded_small_requests: 5, excluded_unknown_requests: 1,
      quality_sampling: { window_known: true, min_input_tokens: 48000, requests: 9, first_token_samples: 8,
        excluded_small_requests: 7, excluded_unknown_requests: 0, bad_windows: 0, good_windows: 0,
        history: [window.qualityWindow(at(-42), { min_input_tokens: 48000, cache_ratio: .335, average_first_token_ms: 1510,
          excluded_small_requests: 5, excluded_unknown_requests: 1, result: 'unknown', reason: '常开组仅观察', models: [
            { model: 'always-fixture', request_type: 'stream', requests: 15, cache_ratio: .335, average_first_token_ms: 1510, first_token_samples: 15 },
          ] })] },
    });
  });
  await poll();
  await unavailable('general').locator('.exception-row.recoverable[data-monitor-account-id="2081"]').waitFor();
  await unavailable('reserve').locator('.exception-row.manual[data-monitor-account-id="2082"]').waitFor();
  const yellow = 'rgb(150, 101, 21)';
  assert(await unavailable('reserve').locator('.exception-row.manual').evaluate((node) => getComputedStyle(node).backgroundColor) === 'rgb(255, 251, 235)' &&
    await monitorButton('2082').evaluate((node) => getComputedStyle(node).color) === yellow, 'manual area and account status use yellow');
  assert(await row('2081').count() === 1 && await row('2082').count() === 1, 'rule and manual exceptions remain listed in account management');
  assert(await monitorButton('2082').innerText() === '用户手动操作' && await monitorButton('2081').locator('.monitor-status-label').innerText() === '可自动恢复' &&
    (await monitorButton('2081').locator('.monitor-retry-countdown').innerText()).startsWith('重试剩余 ') &&
    await monitorButton('2082').locator('.monitor-retry-countdown').count() === 0 &&
    !(await unavailable('reserve').innerText()).includes('运行监控后检测到宿主'), 'unavailable rows use recovery/manual status labels without repeating the full explanation');
  assert(await unavailable('reserve').locator('.monitor-identity').innerText() === '备用 API Key\nID 2082' &&
    (await unavailable('reserve').locator('thead th').allTextContents()).join('|') === '账号|采用倍率|调度情况|缓存比例|采样|平均首字时间|运行状态', 'unavailable rows reuse the complete normal-account format');
  assert(await row('2081').locator('.row-edit').isDisabled() && await row('2082').locator('.row-schedule').isDisabled(), 'exception account settings stay locked while running');
  assert(await monitorRow('2081').count() === 1 && await monitorRow('2081').evaluate((node) =>
    node.classList.contains('monitor-window-row') && [...node.cells].filter((cell) => cell.rowSpan === 3).length === 4),
  'three completed windows share one identity, rate, schedule and state row');
  assert(await historyRows('2081').count() === 2 && (await monitorRow('2081').innerText()).includes('45.1%') &&
    (await historyRows('2081').nth(0).innerText()).includes('92.2%') && (await historyRows('2081').nth(1).innerText()).includes('73.3%') &&
    !(await monitor.innerText()).includes('61.4%'), 'monitor shows latest three completed account windows by window end rather than publication time');
  const windowMetric = (row, name) => row.locator(`.monitor-window-${name}`).innerText();
  const newestCache = await windowMetric(monitorRow('2081'), 'cache');
  const newestSample = await windowMetric(monitorRow('2081'), 'sample');
  assert(newestCache === '45.1%' && newestSample.includes('周期 1 · 异常') && newestSample.includes('–') &&
    (await windowMetric(historyRows('2081').nth(0), 'sample')).includes('周期 2 · 健康') &&
    (await windowMetric(historyRows('2081').nth(1), 'sample')).includes('周期 3 · 未知') &&
    !(await monitor.innerText()).includes('gpt-fixture') && !(await monitor.innerText()).includes('scan-capped-fixture') &&
    await monitorRow('2081').locator('.monitor-identity').innerText() === '主力 API Key\nID 2081', 'cycle labels and times live in sampling cells while cache and account cells omit model names');
  const currentProgress = monitorRow('2081').locator('.monitor-window-sample details.monitor-current-sampling');
  assert(!await currentProgress.evaluate((node) => node.open), 'current progress is collapsed to keep completed cycles compact');
  await currentProgress.locator('summary').click();
  const currentSample = await currentProgress.innerText();
  assert(currentSample.includes('有效 6/15') && currentSample.includes('剔除 3：小请求 2 / 容量未知 1') &&
    !currentSample.includes('有效 0/15') &&
    !currentSample.includes('小请求 9') && !currentSample.includes('gpt-fixture'),
    'sampling-cell progress shows one account window with effective and excluded counts');
  await currentProgress.locator('summary').click();
  assert(
    newestSample.includes('有效 15/15 · ≥40K') && newestSample.includes('剔除 5：小请求 4 / 容量未知 1') &&
    (await windowMetric(historyRows('2081').nth(0), 'sample')).includes('剔除 6：小请求 6 / 容量未知 0') &&
    (await windowMetric(monitorRow('2081'), 'ttft')).includes('902ms') && (await windowMetric(historyRows('2081').nth(0), 'ttft')).includes('812ms') &&
    (await windowMetric(historyRows('2081').nth(1), 'ttft')).includes('样本不足 4/15'), 'completed windows retain their own threshold, exclusions and TTFT independently of live progress');
  assert(await historyRows('2084').count() === 0 && (await monitorRow('2084').innerText()).includes('33.5%') &&
    await monitorButton('2084').locator('.monitor-status-label').innerText() === '可调用',
  'one real always-on window is shown with no padded history or quality pause');
  await monitorButton('2081').click();
  const detail = (field) => ui.locator(`#runtime-detail-${field}`).innerText();
  const accountEvents = ui.locator('#runtime-account-events > li');
  assert(await accountEvents.count() === 5 &&
    JSON.stringify(await accountEvents.locator('p').allTextContents()) === JSON.stringify(['操作 20', '操作 19', '操作 18', '操作 17', '操作 16']) &&
    await ui.locator('#runtime-account-events-empty').isHidden(), 'account details show only the latest five operations, newest first');
  await page.evaluate(() => window.mock.snapshot.events.push(
    { time: new Date().toISOString(), account_id: '2081', action: 'recovery_trial', message: 'ui-secret-canary <em>按原组优先级恢复</em>' },
    { time: new Date().toISOString(), account_id: 2082, action: 'paused', message: '其他账号的操作' }
  ));
  await poll();
  assert(await accountEvents.count() === 5 &&
    await accountEvents.first().locator('.event-action').innerText() === '恢复试运行' &&
    await accountEvents.first().locator('p').innerText() === '[密钥已隐藏] <em>按原组优先级恢复</em>' &&
    (await accountEvents.first().locator('time').innerText()) !== '—' &&
    await accountEvents.locator('em').count() === 0 &&
    !(await ui.locator('#runtime-account-events').innerText()).includes('其他账号的操作'),
    'open account details refresh their own operation time, localized action and safe reason without mixing other accounts');
  checks.push('latest five account operations, empty state, live updates and safe display');
  assert((await detail('recovery')).includes('按原组优先级试运行') && (await detail('next-retry')).includes('本次等待 10 分'), 'paused account shows the next retry and actual wait duration');
  assert(await detail('failure-kind') === '质量异常' && await detail('failure-counts') === '5 / 1' && await detail('recovery-counts') === '4 / 3', 'failure and recovery counts are separate');
  assert(await detail('priority') === '当前 8 · 配置 8' && (await detail('degradation')).includes('2 级 · 仅影响重试间隔，不改组或优先级'), 'backoff tier does not change priority');
  assert(await detail('error-requests') === '宿主暂不提供，不能统计502', 'missing host metrics are not counted as zero');
  assert((await ui.locator('#runtime-sync-error').innerText()).includes('采集 / 同步错误') &&
    await detail('error') === '管理 API 连接失败或超时：[密钥已隐藏] <em>transport</em>' &&
    await ui.locator('#runtime-detail-error em').count() === 0, 'management diagnostics are plain text, redacted and distinct from model-request errors');
  assert((await detail('quality-windows')).includes('异常 2 / 2（至少 3 分）') && (await detail('quality-windows')).includes('健康 0 / 2（至少试运行 5 分）'), 'quality progress includes count and time thresholds');
  const currentWindows = ui.locator('#runtime-quality-current-windows > li');
  const completedWindows = ui.locator('#runtime-quality-windows > li');
  const detailFilter = await detail('filter');
  assert(detailFilter === '≥40K Token' && await completedWindows.first().locator('.runtime-window-model').count() === 2 &&
    (await completedWindows.first().locator('.runtime-window-model').nth(0).innerText()).includes('gpt-fixture · 流式 · 10 次有效') &&
    (await completedWindows.first().locator('.runtime-window-model').nth(1).innerText()).includes('other-model-fixture · 同步 · 5 次有效') &&
    (await completedWindows.first().locator('.runtime-window-model').nth(0).innerText()).includes('首字 900 ms（10/10）'),
    'models are usage details inside a single 15-request account window, with no per-model minimum of 15');
  assert(await completedWindows.count() === 3 &&
    JSON.stringify(await completedWindows.locator('strong').allTextContents()) === JSON.stringify(['窗口 1 · 异常', '窗口 2 · 健康', '窗口 3 · 未知']) &&
    (await completedWindows.nth(0).innerText()).includes('账号窗口整体缓存过低') &&
    (await completedWindows.nth(0).innerText()).includes('剔除 5：小请求 4 / 容量未知 1') &&
    (await completedWindows.nth(1).innerText()).includes('缓存 92.2% · 首字 812 ms') &&
    (await completedWindows.nth(2).innerText()).includes('样本不足 4/15') &&
    !(await ui.locator('#runtime-quality').textContent()).includes('缓存 61.4%'),
    'detail shows the latest three account windows by end time with overall metrics and model details inside');
  assert((await completedWindows.nth(2).innerText()).includes('<img src=x onerror=alert(1)> [密钥已隐藏]') &&
    (await completedWindows.nth(2).innerText()).includes('首字 均值未知（4/15）') &&
    await completedWindows.nth(2).locator('img').count() === 0, 'window model labels are safe and unavailable averages retain the actual sample count');
  assert(!await ui.locator('#runtime-quality-current').evaluate((node) => node.open), 'unfinished windows are collapsed separately from completed history');
  await ui.locator('#runtime-quality-current-summary').click();
  assert(await currentWindows.count() === 1 && (await currentWindows.nth(0).innerText()).includes('当前窗口 · 采样中') &&
    (await currentWindows.nth(0).innerText()).includes('有效 6/15') &&
    (await currentWindows.nth(0).innerText()).includes('剔除 3：小请求 2 / 容量未知 1') &&
    (await currentWindows.nth(0).innerText()).includes('gpt-fixture · 流式 · 4 次有效') &&
    (await currentWindows.nth(0).innerText()).includes('other-model-fixture · 同步 · 2 次有效'),
    'current account window totals six requests across model usage details and keeps its own exclusions');
  const timestamps = await page.evaluate(() => Object.fromEntries(['status_since', 'last_failure_at', 'last_recovery_attempt_at']
    .map((name) => [name, new Date(window.mock.snapshot.accounts[0][name]).toLocaleString('zh-CN', { hour12: false })])));
  assert(await detail('status-since') === timestamps.status_since && await detail('last-failure') === timestamps.last_failure_at &&
    await detail('last-attempt') === timestamps.last_recovery_attempt_at && (await currentWindows.nth(0).innerText()).includes(timestamps.status_since), 'status, failure, attempt and evaluation timestamps come from the snapshot');
  assert((await ui.locator('#runtime-quality-scan-limit').innerText()).includes('部分窗口可能未列出') &&
    await ui.locator('#runtime-quality-scan-limit').isVisible(), 'bounded quality scan warns that some windows may be omitted');
  await page.evaluate(() => { window.mock.snapshot.accounts[0].quality_sampling.window_known = false; });
  await poll();
  const unknownWindow = await currentWindows.first().innerText();
  assert(unknownWindow.includes('当前窗口 · 未知') && unknownWindow.includes('进度与剔除数未知') &&
    !unknownWindow.includes('有效 0/15') && !unknownWindow.includes('小请求 0') &&
    await currentWindows.first().locator('.runtime-window-model').count() === 0,
    'scan-capped current account window shows unknown and hides unverified model counts');
  await page.evaluate(() => { window.mock.snapshot.accounts[0].quality_sampling.window_known = true; });
  await poll();
  const retryBefore = await detail('next-retry');
  const tableRetryBefore = await monitorButton('2081').locator('.monitor-retry-countdown').innerText();
  const requestsBeforeCountdown = await page.evaluate(() => window.mock.requests.length);
  await page.clock.runFor(2100);
  assert(await detail('next-retry') !== retryBefore && await monitorButton('2081').locator('.monitor-retry-countdown').innerText() !== tableRetryBefore &&
    await page.evaluate(() => window.mock.requests.length) === requestsBeforeCountdown, 'detail and status-column countdowns update locally without a new request');
  for (const width of [1180, 780, 390, 320]) {
    await page.setViewportSize({ width, height: 871 });
    assert(await inner.evaluate(() => {
      const dialog = document.querySelector('#runtime-detail-dialog');
      const box = dialog.getBoundingClientRect();
      return box.left >= 0 && box.right <= innerWidth && dialog.scrollWidth <= dialog.clientWidth + 1;
    }), `runtime detail content fits at ${width}`);
  }
  await page.setViewportSize({ width: 1180, height: 900 });
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
  await page.evaluate(() => Object.assign(window.mock.snapshot.accounts[0], {
    state: 'recovery_wait', status_since: new Date().toISOString(), next_retry_at: new Date(Date.now() - 10000).toISOString(), reason: '重试到期，等待前序普通组',
  }));
  await poll();
  await monitorButton('2081').getByText('重试到期 · 等待前序组').waitFor();
  assert(await monitorButton('2081').locator('.monitor-retry-countdown').innerText() === '重试已到期', 'expired status-column countdown remains distinguishable from an active cooldown');
  await monitorButton('2081').click();
  assert((await detail('next-retry')).includes('已到期') && (await detail('recovery')).includes('暂不开始试运行'), 'due recovery waiting is not shown as another cooldown or trial');
  await ui.locator('[data-close-runtime]').last().click();
  await page.evaluate(() => {
    const [main, backup] = window.mock.snapshot.accounts;
    const recoveryAttemptAt = Date.now();
    Object.assign(main, { state: 'recovering', schedulable: true, recovery_trial: true, next_retry_at: null,
      quality_good_windows: 1, recovery_started_at: new Date().toISOString(), recovery_deadline: new Date(Date.now() + 900000).toISOString(),
      reason: '恢复试运行：后备组保持开启' });
    Object.assign(backup, { state: 'ready', schedulable: true, priority: 9, reason: '' });
    Object.assign(window.mock.snapshot.accounts[3], { state: 'temporary_retry', schedulable: false, failure_kind: 'temporary',
      last_recovery_attempt_at: new Date(recoveryAttemptAt).toISOString(), next_retry_at: new Date(recoveryAttemptAt + 320000).toISOString(),
      temporary_failures: 2, recovery_attempts: 2, recovery_failures: 1, degradation_level: 1, error_metrics_available: false, error_requests: 0 });
  });
  await poll();
  await monitorButton('2081').getByText('原组恢复试运行').waitFor();
  assert(await monitorButton('2081').locator('.monitor-retry-countdown').count() === 0, 'cooldown countdown clears when recovery trial starts');
  assert(await schedule('2081').innerText() === '调度中' && await schedule('2082').innerText() === '调度中' &&
    await ui.locator('#overview-callable').innerText() === '2', 'trial and lower fallback are both shown as callable');
  await monitorButton('2081').click();
  assert((await detail('recovery')).includes('原组配置优先级 8') && (await detail('recovery')).includes('较低兜底组保持开启') &&
    (await detail('quality-windows')).includes('健康 1 / 2') && await detail('recovery-start') !== '—' && await detail('recovery-deadline') !== '—',
    'trial shows original priority, start/deadline and live healthy-window progress');
  await ui.locator('[data-close-runtime]').last().click();
  await monitorButton('2084').getByText('临时异常 · 退避重试').waitFor();
  assert((await monitorButton('2084').locator('.monitor-retry-countdown').innerText()).startsWith('重试剩余 '), 'always-on temporary recovery also shows its countdown in the status column');
  await monitorButton('2084').click();
  assert(await detail('group') === '常开组 · 独立并行' && await detail('priority') === '当前 5 · 配置 5' &&
    await detail('quality-windows') === '常开组不执行质量规则' && await completedWindows.count() === 1 &&
    (await completedWindows.first().innerText()).includes('常开组仅观察') &&
    (await completedWindows.first().innerText()).includes('有效 15/15 · ≥48K') &&
    (await completedWindows.first().innerText()).includes('always-fixture · 流式 · 15 次有效') &&
    (await detail('next-retry')).includes('本次等待 5 分 20 秒') && await detail('error-requests') === '宿主暂不提供，不能统计502',
    'always-on temporary faults show actual retry base, observational history, no quality enforcement or false zero errors');
  await ui.locator('[data-close-runtime]').last().click();
  await page.evaluate(() => Object.assign(window.mock.snapshot.accounts[3], {
    state: 'account_error', status: 'error', failure_kind: 'account', next_retry_at: null, reason: '账号状态为 error',
  }));
  await poll();
  await monitorButton('2084').getByText('账号异常 · 需人工处理').waitFor();
  assert(await unavailable('always_on').locator('.exception-row.account-error').count() === 1, 'host account error is not a manual-change row');
  await monitorButton('2084').click();
  assert(await detail('failure-kind') === '账号异常' && await detail('next-retry') === '—' &&
    (await detail('recovery')).includes('不会自动清除宿主账号错误'), 'host account errors never promise auto-clear or recovery retry');
  await ui.locator('[data-close-runtime]').last().click();
  checks.push('account windows with per-model usage, failure and retry metrics; original-priority trial; manual account-error handling');
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
    assert(await inner.evaluate(() => [...document.querySelectorAll('#monitor-group-list .monitor-table')].every((table) => {
      const header = table.querySelector('th:last-child');
      return header.getBoundingClientRect().width >= 190 && [...table.querySelectorAll('.monitor-status')].every((button) => {
        const label = button.querySelector('.monitor-status-label');
        return button.scrollWidth <= button.clientWidth + 1 && label.getBoundingClientRect().height <= 2 * parseFloat(getComputedStyle(label).lineHeight) + 1;
      });
    })), `wide status columns keep long states readable within two lines at ${width}`);
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
  await monitorButton('2081').click();
  assert(await accountEvents.count() === 0 && await ui.locator('#runtime-account-events-empty').isVisible(), 'cleared operation records are also hidden in account details');
  await ui.locator('[data-close-runtime]').last().click();
  checks.push('clear operation records');
  assert(await row('2082').locator('.row-remove').isDisabled(), 'account removal locks while automation is enabled');
  await save(() => ui.locator('#automation-toggle').click());
  assert(!(await saved()).automation_enabled, 'automation can be closed to unlock settings');
  assert(!(await monitor.innerText()).match(/850ms|78\.0%|15 \/ 15/), 'stop hides usage even if the host keeps returning the active snapshot');
  assert(await monitor.locator('.monitor-history-row').count() === 0 && !(await monitor.innerText()).includes('45.1%'), 'stop immediately discards completed-window history for all groups');
  assert(await monitor.locator('.exception-row').count() === 0, 'stop clears runtime exceptions even while the previous snapshot is returned');
  assert(await row('2081').count() === 1 && await row('2082').count() === 1 && !(await row('2081').locator('.row-edit').isDisabled()) && !(await row('2082').locator('.row-schedule').isDisabled()), 'all managed accounts remain editable after stop');
  await monitorButton('2081').click();
  assert(await ui.locator('#runtime-detail-cache').innerText() === '未采集' && await ui.locator('#runtime-detail-ttft').innerText() === '未采集', 'stop also hides metrics in reopened details');
  assert((await ui.locator('#runtime-detail-status').innerText()) === '未启用管理' && await ui.locator('#runtime-detail-recovery').evaluate((node) => node.parentElement.hidden), 'stopped details clear runtime state and recovery countdown');
  assert(await ui.locator('#runtime-diagnostics').isHidden() && await currentWindows.count() === 0 && await completedWindows.count() === 0 &&
    await detail('next-retry') === '—' && await detail('error-requests') === '宿主暂不提供，不能统计502' &&
    await ui.locator('#runtime-sync-error').isHidden() && await ui.locator('#runtime-quality-scan-limit').isHidden(),
    'stop clears prior timestamps, trial, retry, sync errors and bucket diagnostics');
  await ui.locator('[data-close-runtime]').last().click();
  assert(!(await ui.locator('#save-rules').isDisabled()) && !(await ui.locator('#reset-rules').isDisabled()) && !(await ui.locator('#admin-url').isDisabled()) &&
    await monitor.locator('.monitor-retry-countdown').count() === 0, 'settings unlock and status countdowns clear after automation is disabled');
  await save(() => ui.locator('#automation-toggle').click());
  await poll();
  assert(await monitor.locator('.exception-row.manual, .exception-row.recoverable').count() === 0 && !(await monitor.innerText()).includes('缓存命中低于下限'), 're-enable immediately clears old manual and paused states despite old-session snapshot');
  assert(await monitor.locator('.monitor-history-row').count() === 0 && !(await monitor.innerText()).includes('33.5%'), 're-enable hides previous-session ordinary and always-on history');
  await monitorButton('2081').click();
  assert(await ui.locator('#runtime-diagnostics').isHidden() && await currentWindows.count() === 0 && await completedWindows.count() === 0 && await ui.locator('#runtime-sync-error').isHidden(), 're-enable does not reveal previous-session details');
  await ui.locator('[data-close-runtime]').last().click();
  await page.evaluate(() => {
    Object.assign(window.mock.snapshot, { sampling_since: new Date().toISOString(), polling: false });
    for (const account of window.mock.snapshot.accounts) Object.assign(account, { state: 'sampling', schedulable: account.id === 2081,
      reason: '', error: '', availability_reason: '', next_retry_at: null, recovery_trial: false, quality_sampling: null, quality_scan_limit_reached: false, requests: 0, cache_ratio: null, complete: false,
      average_first_token_ms: null, first_token_samples: 0, excluded_small_requests: 0, excluded_unknown_requests: 0 });
  });
  await poll();
  await monitorButton('2081').getByText('等待采样 · 可调用').waitFor();
  assert(!(await monitor.innerText()).includes('缓存命中低于下限') && await monitor.locator('.exception-row.manual, .exception-row.recoverable').count() === 0, 'new session begins with fresh sampling state');
  assert(await monitor.locator('.monitor-history-row').count() === 0 && !(await monitor.innerText()).includes('45.1%'), 'new session does not restore old completed windows');
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
  await ui.locator('.section-nav a[href="#view-monitor"]').click();
  await page.clock.runFor(1500);
  for (const width of [1440, 1180, 1050, 980, 800, 781, 780, 766, 480, 390, 320]) {
    await page.setViewportSize({ width, height: 871 });
    assert(await inner.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `no page overflow at ${width}`);
    assert(await inner.evaluate(() => {
      const nav = document.querySelector('.section-nav');
      const links = nav.querySelector('.section-nav-links');
      links.scrollLeft = links.scrollWidth;
      const button = document.querySelector('#automation-toggle').getBoundingClientRect();
      const version = document.querySelector('#top-version').getBoundingClientRect();
      return [button, version].every((box) => box.left >= 0 && box.right <= document.documentElement.clientWidth) &&
        Math.abs(button.right - (nav.getBoundingClientRect().right - parseFloat(getComputedStyle(nav).paddingRight))) < 1;
    }), `monitor CTA stays pinned to the right and version remains visible at ${width}`);
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
  const configBeforeReset = await saved();
  await ui.locator('#platform-select').selectOption('anthropic');
  assert(await excellentThreshold.inputValue() === '52000', 'previously saved Anthropic threshold remains intact before editing a draft');
  await excellentThreshold.fill('71000');
  await field('excellent', 'priority').fill('71');
  await ui.locator('#platform-select').selectOption('openai');
  await field('good', 'cache-enabled').uncheck();
  assert(await field('good', 'cache-enabled').evaluate((node) => node.closest('.check-line').classList.contains('rule-customized')), 'changed condition toggles are highlighted too');
  await ui.locator('#reset-rules').click();
  assert(await field('always_on', 'priority').inputValue() === '5' && await field('always_on', 'retry-seconds').inputValue() === '300' &&
    await field('excellent', 'priority').inputValue() === '6' && await field('excellent', 'cache-minimum').inputValue() === '80' &&
    await field('excellent', 'ttft-maximum').inputValue() === '10000' && await field('excellent', 'confirmation-seconds').inputValue() === '180' &&
    await field('excellent', 'recovery-seconds').inputValue() === '300' && await field('good', 'cache-enabled').isChecked() &&
    (await tokenFields.evaluateAll((inputs) => inputs.map((input) => input.value))).every((value) => value === '40000') &&
    await ui.locator('[data-rule-group] .rule-customized').count() === 0, 'restore resets all current-platform group parameters and removes green highlights');
  assert(JSON.stringify(await saved()) === JSON.stringify(configBeforeReset), 'restoring defaults leaves the saved configuration intact until save');
  await save(() => ui.locator('#save-rules').click());
  const configAfterReset = await saved();
  assert(configAfterReset.platform_group_priorities.openai.excellent === 6 && configAfterReset.platform_group_rules.openai.excellent.cooldown_seconds === 300 &&
    configAfterReset.platform_group_rules.openai.excellent.min_input_tokens === 40000 && configAfterReset.platform_group_rules.openai.always_on.min_input_tokens === 40000 &&
    JSON.stringify(configAfterReset.platform_group_rules.anthropic) === JSON.stringify(configBeforeReset.platform_group_rules.anthropic) &&
    JSON.stringify(configAfterReset.platform_group_priorities.anthropic) === JSON.stringify(configBeforeReset.platform_group_priorities.anthropic) &&
    configAfterReset.admin_url === configBeforeReset.admin_url && configAfterReset.poll_seconds === configBeforeReset.poll_seconds &&
    JSON.stringify(configAfterReset.account_settings) === JSON.stringify(configBeforeReset.account_settings), 'saved defaults affect the selected platform groups while preserving other configuration');
  await ui.locator('#platform-select').selectOption('anthropic');
  assert(await excellentThreshold.inputValue() === '71000' && await field('excellent', 'priority').inputValue() === '71' &&
    (await saved()).platform_group_rules.anthropic.excellent.min_input_tokens === 52000 &&
    (await saved()).platform_group_priorities.anthropic.excellent === 60, 'saving the OpenAI restore preserves but never submits the unsaved Anthropic draft');
  await ui.locator('#platform-select').selectOption('openai');
  await field('excellent', 'retry-seconds').fill('301');
  await field('excellent', 'retry-seconds').fill('300');
  assert(!await field('excellent', 'retry-seconds').evaluate((node) => node.classList.contains('rule-customized')), 'manually returning to a default also clears the green highlight');
  checks.push('current-platform restore, unsaved cross-platform draft isolation, and customized token thresholds');
  checks.push('latest three completed windows, separate live progress, per-window exclusions, and session-bound history');
  await ui.locator('#view-rules').scrollIntoViewIfNeeded();
  checks.push('responsive layout and no JS errors');
  return { passed: checks };
})
