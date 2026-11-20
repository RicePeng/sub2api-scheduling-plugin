(() => {
  'use strict';

  const groups = [
    { id: 'always_on', name: '常开', priority: 5, color: '#a74439', soft: '#f8eae7', alwaysOn: true },
    { id: 'excellent', name: '优秀', priority: 6, color: '#14775f', soft: '#e7f3ee', special: true },
    { id: 'good', name: '良好', priority: 7, color: '#2e7b83', soft: '#e8f2f2' },
    { id: 'general', name: '一般', priority: 8, color: '#966515', soft: '#f7f0df' },
    { id: 'reserve', name: '备用', priority: 9, color: '#446f99', soft: '#ebf1f7' },
    { id: 'temporary', name: '临时', priority: 99, color: '#a64c2c', soft: '#f8ece6', callable: false },
  ];
  const callGroups = groups.filter((group) => group.callable !== false);
  const qualityWindowSize = 15;
  const confirmWindows = (count) => Math.max(2, Number(count) || 2);
  const groupPriorityDefaults = Object.fromEntries(groups.map(({ id, priority }) => [id, priority]));
  const knownPlatforms = ['openai', 'anthropic', 'gemini', 'vertex', 'antigravity', 'grok', 'deepseek', 'qwen', 'moonshot', 'zhipu', 'minimax', 'siliconflow', 'ollama', 'openrouter', 'openai_compatible'];
  const platformNames = { openai: 'OpenAI', anthropic: 'Anthropic', gemini: 'Gemini', vertex: 'Vertex', antigravity: 'Antigravity', grok: 'Grok', deepseek: 'DeepSeek', qwen: 'Qwen', moonshot: 'Moonshot', zhipu: '智谱', minimax: 'MiniMax', siliconflow: '硅基流动', ollama: 'Ollama', openrouter: 'OpenRouter', openai_compatible: 'OpenAI Compatible', unknown: '待识别' };
  const defaultRule = (group) => ({ min_cache_ratio: .8, max_average_first_token_ms: 10000, exclusive_rate_below: group === 'excellent' ? .05 : 0,
    exclusive_min_cache_ratio: .8, exclusive_max_average_first_token_ms: 10000, max_rate_multiplier: .2,
    min_requests: qualityWindowSize, min_input_tokens: 40000, trigger_samples: 2, recovery_samples: 2, confirmation_seconds: 180, recovery_seconds: 300,
    cooldown_seconds: 300, action: 'pause', priority_penalty: 10 });

  let accounts = [];
  let activePlatform = 'unknown';
  let baseGroupPriorities = { ...groupPriorityDefaults };
  let platformGroupPriorities = {};
  let groupRules = Object.fromEntries(groups.map((group) => [group.id, defaultRule(group.id)]));
  let platformGroupRules = {};
  let platformSelected = false;

  const $ = (id) => document.getElementById(id);
  const groupFor = (id) => groups.find((group) => group.id === id);
  const number = (value) => Number.isFinite(value) ? `${value.toLocaleString('en-US', { minimumFractionDigits: 1, maximumFractionDigits: 6, useGrouping: false })}×` : '未知';
  const sortedGroups = () => groups;
  let draggedAccountId = null;
  const collapsedGroups = new Set();
  let editingAccount = null;
  let removingAccount = null;
  let automationEnabled = false;
  let samplingActive = false;
  let samplingRevision = 0;
  let samplingSession = '';
  let resetSessionPending = false;
  let runtimePolling = false;
  let nextPollAt = null;
  let manualRequestBusy = false;
  let countdownTimer = null;
  let detailAccountId = null;
  let configLoaded = false;
  let runtimeReady = false;
  let busy = false;
  let statusBusy = false;
  let disposed = false;
  let savedConfig = null;
  let runtimeHostname = '';
  let sequence = 0;
  let pollTimer = null;
  let eventLog = [];
  const hiddenEventKeys = new Set();
  let eventPage = 1;
  const eventPageSize = 10;
  const pending = new Map();
  const secrets = new Set();
  const token = new URLSearchParams(window.location.hash.slice(1)).get('bridge_token') || '';
  const available = token !== '' && window.parent !== window;
  const defaults = { admin_url: '', admin_api_key: '', account_ids: [], poll_seconds: 60, window_minutes: 15,
    timeout_seconds: 15, upstream_rates_enabled: true, automation_enabled: false, executor_hostname: '',
    account_settings: [], group_priorities: { ...groupPriorityDefaults },
    group_rules: Object.fromEntries(groups.map((group) => [group.id, defaultRule(group.id)])), platform_group_priorities: {},
    platform_group_rules: {}, rule: defaultRule('general') };
  const setText = (id, value) => { const element = $(id); if (element) element.textContent = value; };
  const sectionNav = document.querySelector('.section-nav');
  const sectionNavLinks = [...document.querySelectorAll('.section-nav a')];
  // CSS places the monitor view before the management view. Active-section
  // detection must use rendered document order rather than source order.
  const sectionNavSections = sectionNavLinks.map((link) => $(link.hash.slice(1))).filter(Boolean)
    .sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top);
  let navTarget = '';

  const visibleAccounts = () => accounts.filter((account) => account.platform === activePlatform);
  const groupPriority = (id, platform = activePlatform) => id === 'temporary' ? 99 : platformGroupPriorities[platform]?.[id] ?? baseGroupPriorities[id];
  const currentRule = (group, platform = activePlatform) => {
    const rule = { ...groupRules[group], ...platformGroupRules[platform]?.[group] };
    return { ...rule, min_input_tokens: rule.min_input_tokens || 40000, confirmation_seconds: rule.confirmation_seconds || 180, recovery_seconds: rule.recovery_seconds || 300 };
  };
  const stateLabels = { paused: '异常停用', deprioritized: '异常停用', temporary: '临时组不调用', temporary_retry: '恢复退避中', recovery_wait: '重试到期 · 等待前序组', sampling: '等待采样',
    needs_rate: '待设置倍率', group_wait: '等待前序组', exclusive_wait: '独占等待', unavailable: '不可调用',
    account_error: '账号异常需人工处理', manual_change: '人工变更', unsupported: '不支持的账号', unknown: '等待采集', recovering: '恢复试运行', pending: '异常确认中',
    ready: '可调用', observing: '仅监控' };
  function accountState(account) {
    if (account.type && account.type !== 'apikey') return '不支持的账号';
    if (!automationEnabled) return account.group === 'temporary' ? '临时组 · 待启用管理'
      : account.desiredSchedulable === false ? '已停止调用' : '未启用管理';
    if (account.state === 'account_error') return '账号异常 · 需人工处理';
    if (account.state === 'manual_change') return `手动操作 · 调度${account.schedulable ? '开启' : '关闭'}`;
    if (account.group === 'temporary' && account.status !== 'exception') return automationEnabled ? '临时组不调用' : '临时组 · 待启用管理';
    if (account.desiredSchedulable === false) return '已停止调用';
    if (account.state === 'unavailable') return '不可调用';
    if (account.status === 'exception') return account.reason || '异常停用';
    if (account.state === 'recovering') return '恢复试运行';
    if (account.state === 'pending') return '异常确认中';
    if (account.state === 'unsupported') return '不支持的账号';
    if (effectiveRate(account) === null) return '待设置倍率';
    return stateLabels[account.state] || '等待采集';
  }
  function renderPlatformOptions() {
    const detected = accounts.map((account) => account.platform).filter((platform) => platform !== 'unknown');
    if (!platformSelected && activePlatform === 'unknown' && !accounts.some((account) => account.platform === 'unknown') && detected.length) activePlatform = detected[0];
    const platforms = [...new Set([...knownPlatforms, ...Object.keys(platformGroupRules), ...Object.keys(platformGroupPriorities), ...accounts.map((account) => account.platform), activePlatform])];
    $('platform-select').replaceChildren(...platforms.map((platform) => new Option(platformNames[platform] || platform, platform)));
    $('platform-select').value = activePlatform;
  }
  function renderRuleEditor() {
    setText('rules-platform', `${platformNames[activePlatform] || activePlatform} · 五组独立设置`);
    $('group-rules-list').replaceChildren(...callGroups.map((group, index) => {
      const rule = currentRule(group.id);
      const card = document.createElement('section');
      card.className = 'rule-section group-rule-card';
      card.dataset.ruleGroup = group.id;
      card.style.setProperty('--group-color', group.color);
      card.innerHTML = `<div class="rule-section-head"><div><h3>${String(index + 1).padStart(2, '0')} ${group.name}组</h3><p>${group.alwaysOn ? '独立调用 · 不执行质量规则' : '独立优先级、异常确认与恢复'}</p></div><label class="group-priority"><span>组内优先级</span><input data-field="priority" type="number" min="0" max="10000" step="1" aria-label="${group.name}组优先级"></label></div>
        <div class="setting-row setting-row-input"><div><strong>最小输入上下文</strong><span>输入、缓存创建与缓存读取 Token 合计达到此值才采样；40K = 40000 Token。</span></div><div class="unit-input"><input data-field="min-input-tokens" type="number" min="1" max="1000000000" step="1" aria-label="${group.name}组最小采样输入上下文"><span>Token</span></div></div>
        ${group.alwaysOn ? '' : `
        <div class="condition-row"><label class="check-line"><input data-field="rate-enabled" type="checkbox" aria-label="${group.name}组启用倍率上限条件"><span>有效倍率超过上限</span></label><div class="unit-input"><input data-field="rate-maximum" type="number" min="0.01" max="1000000" step="0.01" aria-label="${group.name}组有效倍率上限"><span>×</span></div></div>
        <div class="condition-row"><label class="check-line"><input data-field="cache-enabled" type="checkbox" aria-label="${group.name}组启用缓存条件"><span>缓存比例低于下限</span></label><div class="unit-input"><input data-field="cache-minimum" type="number" min="0.1" max="100" step="0.1" aria-label="${group.name}组缓存下限"><span>%</span></div></div>
        <div class="condition-row"><label class="check-line"><input data-field="ttft-enabled" type="checkbox" aria-label="${group.name}组启用首字时间条件"><span>平均首字时间超过上限</span></label><div class="unit-input"><input data-field="ttft-maximum" type="number" min="1" max="3600000" step="1" aria-label="${group.name}组首字时间上限"><span>ms</span></div></div>
        <div class="setting-row setting-row-input"><div><strong>连续异常窗口</strong><span>账号窗口总体指标至少连续异常 2 次才停用。</span></div><div class="unit-input"><input data-field="trigger-samples" type="number" min="2" max="100" step="1" aria-label="${group.name}组连续异常窗口数"><span>个</span></div></div>
        <div class="setting-row setting-row-input"><div><strong>异常确认时长</strong><span>启动监控后先等待此时长；确认质量异常时，异常窗口的时间跨度也须达到此时长。</span></div><div class="unit-input"><input data-field="confirmation-seconds" type="number" min="60" max="86400" step="1" aria-label="${group.name}组异常确认时长"><span>秒</span></div></div>
        <div class="setting-row setting-row-input"><div><strong>恢复确认窗口</strong><span>试运行须通过至少 2 个连续健康账号窗口。</span></div><div class="unit-input"><input data-field="recovery-samples" type="number" min="2" max="100" step="1" aria-label="${group.name}组恢复确认窗口数"><span>个</span></div></div>
        <div class="setting-row setting-row-input"><div><strong>恢复确认时长</strong><span>达到健康窗口数和最短试运行时长后才确认恢复。</span></div><div class="unit-input"><input data-field="recovery-seconds" type="number" min="60" max="86400" step="1" aria-label="${group.name}组恢复确认时长"><span>秒</span></div></div>`}
        <div class="setting-row setting-row-input"><div><strong>失败重试基准</strong><span>默认 300 秒；连续失败后间隔每次加倍，最长 24 小时。</span></div><div class="unit-input"><input data-field="retry-seconds" type="number" min="30" max="86400" step="1" aria-label="${group.name}组失败重试基准（秒）"><span>秒</span></div></div>
        ${group.special ? '<div class="condition-row"><label class="check-line"><input data-field="elite-enabled" type="checkbox" aria-label="优秀组启用独占策略"><span>倍率低于下限时独占调用</span></label><div class="unit-input"><input data-field="elite-minimum" type="number" min="0.01" max="1000000" step="0.01" aria-label="优秀组独占倍率下限"><span>×</span></div></div>' +
          '<div class="setting-row setting-row-input"><div><strong>独占缓存下限</strong><span>缓存低于此值时退出独占。</span></div><div class="unit-input"><input data-field="elite-cache-minimum" type="number" min="0.1" max="100" step="0.1" aria-label="优秀组独占缓存下限"><span>%</span></div></div>' +
          '<div class="setting-row setting-row-input"><div><strong>独占平均首字时间上限</strong><span>最近 15 个有效样本均值超过此值时退出独占。</span></div><div class="unit-input"><input data-field="elite-ttft-maximum" type="number" min="1" max="3600000" step="1" aria-label="优秀组独占首字时间上限"><span>ms</span></div></div>' +
          '<p class="rule-footnote">独占仅保留倍率最低的合格账号。缓存或首字时间触发任一独占条件就退出独占，按本组普通规则继续调用；样本不足时不判失败。</p>' : ''}
        ${group.alwaysOn ? '<p class="rule-footnote">常开组和当前普通调用组可同时开启，不因质量指标停用。立即尝试清除宿主限流、过载或临时不可调度标记；持续或反复失败则退避。宿主账号错误需人工处理，不会自动清除。</p>' : ''}`;
      const ruleDefaults = defaultRule(group.id);
      const fieldDefaults = { priority: group.priority, 'retry-seconds': ruleDefaults.cooldown_seconds, 'min-input-tokens': ruleDefaults.min_input_tokens,
        'rate-enabled': true, 'rate-maximum': ruleDefaults.max_rate_multiplier,
        'cache-enabled': true, 'cache-minimum': ruleDefaults.min_cache_ratio * 100,
        'ttft-enabled': true, 'ttft-maximum': ruleDefaults.max_average_first_token_ms,
        'trigger-samples': ruleDefaults.trigger_samples, 'recovery-samples': ruleDefaults.recovery_samples,
        'confirmation-seconds': ruleDefaults.confirmation_seconds, 'recovery-seconds': ruleDefaults.recovery_seconds,
        'elite-enabled': ruleDefaults.exclusive_rate_below > 0, 'elite-minimum': ruleDefaults.exclusive_rate_below,
        'elite-cache-minimum': ruleDefaults.exclusive_min_cache_ratio * 100, 'elite-ttft-maximum': ruleDefaults.exclusive_max_average_first_token_ms };
      const set = (name, value) => {
        const field = card.querySelector(`[data-field="${name}"]`);
        field.dataset.defaultValue = fieldDefaults[name];
        field.title = `默认值：${field.type === 'checkbox' ? fieldDefaults[name] ? '启用' : '关闭' : fieldDefaults[name]}`;
        if (field.type === 'checkbox') field.checked = value;
        else field.value = value;
      };
      set('priority', groupPriority(group.id));
      set('retry-seconds', rule.cooldown_seconds);
      set('min-input-tokens', rule.min_input_tokens);
      if (group.alwaysOn) return card;
      set('rate-enabled', rule.max_rate_multiplier > 0);
      set('rate-maximum', rule.max_rate_multiplier || .2);
      set('cache-enabled', rule.min_cache_ratio > 0);
      set('cache-minimum', rule.min_cache_ratio > 0 ? Number((rule.min_cache_ratio * 100).toFixed(1)) : 80);
      set('ttft-enabled', rule.max_average_first_token_ms > 0);
      set('ttft-maximum', rule.max_average_first_token_ms || 10000);
      set('trigger-samples', confirmWindows(rule.trigger_samples));
      set('recovery-samples', confirmWindows(rule.recovery_samples));
      set('confirmation-seconds', rule.confirmation_seconds);
      set('recovery-seconds', rule.recovery_seconds);
      if (group.special) {
        set('elite-enabled', rule.exclusive_rate_below > 0);
        set('elite-minimum', rule.exclusive_rate_below || 0.05);
        set('elite-cache-minimum', Number(((rule.exclusive_min_cache_ratio || .8) * 100).toFixed(1)));
        set('elite-ttft-maximum', rule.exclusive_max_average_first_token_ms || 10000);
      }
      return card;
    }));
    updateRuleInputs();
  }
  function updateRuleInputs() {
    for (const card of document.querySelectorAll('[data-rule-group]')) {
      const field = (name) => card.querySelector(`[data-field="${name}"]`);
      for (const [enabled, input] of [['rate-enabled', 'rate-maximum'], ['cache-enabled', 'cache-minimum'], ['ttft-enabled', 'ttft-maximum'], ['elite-enabled', 'elite-minimum'], ['elite-enabled', 'elite-cache-minimum'], ['elite-enabled', 'elite-ttft-maximum']]) {
        if (field(input)) field(input).disabled = !configLoaded || busy || !runtimeReady || automationEnabled || !field(enabled).checked;
      }
      for (const input of card.querySelectorAll('[data-field]')) {
        const customized = input.type === 'checkbox' ? String(input.checked) !== input.dataset.defaultValue
          : !input.value.trim() || Number(input.value) !== Number(input.dataset.defaultValue);
        input.classList.toggle('rule-customized', customized);
        if (input.type === 'checkbox') input.closest('.check-line').classList.toggle('rule-customized', customized);
      }
    }
  }
  function captureRuleEditor() {
    const rules = { ...platformGroupRules[activePlatform] };
    const priorities = { ...platformGroupPriorities[activePlatform], temporary: 99 };
    for (const card of document.querySelectorAll('[data-rule-group]')) {
      const group = card.dataset.ruleGroup;
      const field = (name) => card.querySelector(`[data-field="${name}"]`);
      const fields = ['priority', 'retry-seconds', 'min-input-tokens'];
      if (group !== 'always_on') fields.push('trigger-samples', 'recovery-samples', 'confirmation-seconds', 'recovery-seconds');
      if (field('rate-enabled')?.checked) fields.push('rate-maximum');
      if (field('cache-enabled')?.checked) fields.push('cache-minimum');
      if (field('ttft-enabled')?.checked) fields.push('ttft-maximum');
      if (group === 'excellent' && field('elite-enabled').checked) fields.push('elite-minimum', 'elite-cache-minimum', 'elite-ttft-maximum');
      const invalid = fields.map(field).find((input) => !input.value.trim() || !input.checkValidity());
      if (invalid) { invalid.reportValidity(); throw new Error(`请填写有效的${groupFor(group).name}组异常条件。`); }
      priorities[group] = Number(field('priority').value);
      rules[group] = { ...currentRule(group), cooldown_seconds: Number(field('retry-seconds').value), min_input_tokens: Number(field('min-input-tokens').value) };
      if (group === 'always_on') continue;
      Object.assign(rules[group], { max_rate_multiplier: field('rate-enabled').checked ? Number(field('rate-maximum').value) : 0, action: 'pause',
        min_cache_ratio: field('cache-enabled').checked ? Number(field('cache-minimum').value) / 100 : 0,
        max_average_first_token_ms: field('ttft-enabled').checked ? Number(field('ttft-maximum').value) : 0,
        min_requests: qualityWindowSize, trigger_samples: Number(field('trigger-samples').value),
        recovery_samples: Number(field('recovery-samples').value), confirmation_seconds: Number(field('confirmation-seconds').value),
        recovery_seconds: Number(field('recovery-seconds').value) });
      if (group === 'excellent') {
        rules[group].exclusive_rate_below = field('elite-enabled').checked ? Number(field('elite-minimum').value) : 0;
        rules[group].exclusive_min_cache_ratio = Number(field('elite-cache-minimum').value) / 100;
        rules[group].exclusive_max_average_first_token_ms = Number(field('elite-ttft-maximum').value);
      }
    }
    platformGroupRules[activePlatform] = rules;
    platformGroupPriorities[activePlatform] = priorities;
  }

  function setActiveSection(id) {
    sectionNavLinks.forEach((link) => {
      const active = link.hash === `#${id}`;
      link.classList.toggle('active', active);
      if (active) link.setAttribute('aria-current', 'page');
      else link.removeAttribute('aria-current');
    });
  }

  function updateActiveNav() {
    if (!sectionNav || sectionNavSections.length === 0) return;
    if (navTarget) return;
    const threshold = sectionNav.getBoundingClientRect().bottom + 1;
    const best = sectionNavSections.filter((section) => section.getBoundingClientRect().top <= threshold).at(-1) || sectionNavSections[0];
    setActiveSection(best.id);
  }

  sectionNavLinks.forEach((link) => link.addEventListener('click', () => {
    navTarget = link.hash.slice(1);
    setActiveSection(navTarget);
  }));
  window.addEventListener('hashchange', () => {
    if (!navTarget) updateActiveNav();
  });
  window.addEventListener('scroll', () => { if (!navTarget) updateActiveNav(); }, { passive: true });
  for (const type of ['wheel', 'touchmove']) {
    window.addEventListener(type, () => { navTarget = ''; }, { passive: true });
  }
  window.addEventListener('keydown', (event) => {
    if (['PageDown', 'PageUp', 'Home', 'End', 'ArrowDown', 'ArrowUp', ' '].includes(event.key)) navTarget = '';
  });
  window.addEventListener('resize', updateActiveNav);

  function groupAccounts(groupId) {
    const direction = $('sort-direction').value === 'desc' ? -1 : 1;
    return visibleAccounts().filter((account) => account.group === groupId)
      .sort((a, b) => {
        const aRate = effectiveRate(a);
        const bRate = effectiveRate(b);
        if (aRate === null && bRate === null) return a.id.localeCompare(b.id);
        if (aRate === null) return 1;
        if (bRate === null) return -1;
        return (aRate - bRate) * direction;
      });
  }

  function adoptedRate(account) {
    if (account.rateMode === 'fixed') return { mode: 'fixed', value: account.fixedRate ?? null };
    if (account.rateMode === 'default') return { mode: 'default', value: 1 };
    if (Number.isFinite(account.upstreamRate)) return { mode: 'upstream', value: account.upstreamRate };
    return { mode: 'default', value: 1 };
  }
  const effectiveRate = (account) => adoptedRate(account).value;

  function makeIdentity(account, draggable = false) {
    const identity = document.createElement('div');
    identity.className = 'account-identity';
    if (draggable) {
      const grip = document.createElement('span');
      grip.className = 'account-drag-grip';
      grip.title = '拖动账号到其他分组';
      grip.textContent = '↕';
      identity.append(grip);
    }
    const avatar = document.createElement('span');
    avatar.className = 'avatar-mark';
    avatar.textContent = account.letter || account.name.slice(0, 1).toUpperCase();
    const labels = document.createElement('span');
    const name = document.createElement('strong');
    name.textContent = account.name;
    const id = document.createElement('small');
    id.textContent = `ID ${account.id} · ${platformNames[account.platform] || account.platform} · ${account.type && account.type !== 'apikey' ? '不支持的账号' : 'API Key'}`;
    labels.append(name, id);
    identity.append(avatar, labels);
    return identity;
  }

  function makeRatePair(account) {
    const pair = document.createElement('div');
    pair.className = 'rate-pair';
    const { mode, value: rate } = adoptedRate(account);
    const source = document.createElement('span');
    source.textContent = { default: '上游不支持', waiting: '上游', upstream: '上游', fixed: '固定' }[mode];
    const value = document.createElement('strong');
    value.textContent = mode === 'waiting' ? '等待获取' : mode === 'default' ? `默认 ${number(rate)}` : number(rate);
    pair.classList.add(mode);
    pair.append(source, value);
    return pair;
  }

  function isUnavailable(account) {
    return account.state === 'unsupported' || automationEnabled && !isCallable(account);
  }

  function renderMonitorGroups() {
    const collecting = automationEnabled && samplingActive;
    setText('monitor-poll-label', collecting ? `每 ${savedConfig.poll_seconds} 秒采集` : automationEnabled ? '等待采样启动' : '采样未启用');
    const monitoredGroups = groups
      .map((group, index) => ({ group, rank: index, members: groupAccounts(group.id) }))
      .filter(({ members }) => members.length > 0);
    renderAccountOverview(monitoredGroups.flatMap(({ members }) => members));
    renderRouteOrder(monitoredGroups.filter(({ group }) => group.callable !== false));
    $('monitor-empty').hidden = monitoredGroups.length > 0;
    $('monitor-group-list').replaceChildren(...monitoredGroups.map(({ group, members }) => {
      const unavailable = members.filter(isUnavailable);
      const normal = members.filter((account) => !isUnavailable(account));
      const card = document.createElement('section');
      card.className = 'monitor-group-card';
      card.dataset.monitorGroup = group.id;
      card.style.setProperty('--group-color', group.color);
      card.style.setProperty('--group-soft', group.soft);
      const header = document.createElement('header');
      const title = document.createElement('h3');
      title.textContent = group.name;
      const count = document.createElement('span');
      count.textContent = group.callable === false ? `${members.length} 个账号 · 不参与自动调用` : `${normal.filter(isCallable).length} / ${members.length} 可调用`;
      header.append(title, count);
      card.append(header);
      if (normal.length) {
        const table = $('monitor-table-template').content.cloneNode(true);
        table.querySelector('table').setAttribute('aria-label', `${group.name}组账号监控`);
        table.querySelector('tbody').append(...normal.map((account) => renderMonitorRow(account, collecting)));
        card.append(table);
      } else {
        const empty = document.createElement('p');
        empty.className = 'monitor-group-empty';
        empty.textContent = automationEnabled ? '当前组暂无参与调用的账号' : '暂无可展示账号';
        card.append(empty);
      }
      card.append(renderExceptionArea(unavailable, collecting));
      return card;
    }));
  }

  function renderMonitorRow(account, collecting, unavailable = false) {
    const history = collecting ? monitorWindowHistory(account) : [];
    const currentWindows = collecting ? monitorCurrentWindows(account) : [];
    const row = document.createElement('tr');
    const identityCell = document.createElement('td');
    const identity = document.createElement('span');
    identity.className = 'monitor-identity';
    const name = document.createElement('strong');
    name.textContent = account.name;
    const meta = document.createElement('small');
    meta.textContent = `ID ${account.id}`;
    identity.append(name, meta);
    identityCell.append(identity);

    const rateCell = document.createElement('td');
    rateCell.append(makeRatePair(account));

    const scheduleCell = document.createElement('td');
    scheduleCell.append(makeScheduleStatus(account));

    const [cacheCell, sampleCell, ttftCell] = makeMonitorWindowCells(account, collecting, history[0] || currentWindows[0], 1);
    if ((history.length && currentWindows.length) || currentWindows.length > 1) {
      const current = document.createElement('details');
      current.className = 'monitor-current-sampling';
      const summary = document.createElement('summary');
      summary.textContent = '当前进度';
      current.append(summary);
      currentWindows.forEach((window) => {
        const progress = document.createElement('small');
        progress.textContent = window.window_known === false ? '进度未知' : `有效 ${count(window.requests)}/${qualityWindowSize}`;
        current.append(progress);
        if (window.window_known !== false) current.append(makeSamplingExclusions(window));
      });
      sampleCell.append(current);
    }

    const statusCell = document.createElement('td');
    const runtimeState = runtimeGroupState(account);
    if (unavailable) {
      row.className = `exception-row ${runtimeState.key === 'exception' ? 'recoverable' : runtimeState.key}`;
      runtimeState.label = ({ exception: '可自动恢复', manual: '用户手动操作' })[runtimeState.key] || runtimeState.label;
    }
    row.classList.add('monitor-window-row');
    row.dataset.monitorAccountId = account.id;
    const status = document.createElement('button');
    status.type = 'button';
    status.className = `monitor-status ${runtimeState.key}`;
    const statusLabel = document.createElement('span');
    statusLabel.className = 'monitor-status-label';
    statusLabel.textContent = runtimeState.label;
    status.append(statusLabel);
    status.title = `${runtimeState.label} · 点击查看运行详情`;
    status.setAttribute('aria-label', `${account.name}：${runtimeState.label}，查看运行详情`);
    if (automationEnabled && samplingActive && !resetSessionPending &&
      ['paused', 'deprioritized', 'temporary_retry', 'recovery_wait'].includes(account.state) && Number.isFinite(account.diagnostics?.nextRetryAt)) {
      const countdown = document.createElement('span');
      countdown.className = 'monitor-retry-countdown';
      countdown.dataset.retryAt = account.diagnostics.nextRetryAt;
      status.append(countdown);
    }
    status.addEventListener('click', () => showRuntimeDetails(account));
    statusCell.append(status);
    row.append(identityCell, rateCell, scheduleCell, cacheCell, sampleCell, ttftCell, statusCell);
    const countdown = status.querySelector('.monitor-retry-countdown');
    if (countdown) updateMonitorRetry(countdown);
    if (history.length < 2) return row;
    for (const cell of [identityCell, rateCell, scheduleCell, statusCell]) cell.rowSpan = history.length;
    const rows = document.createDocumentFragment();
    rows.append(row);
    history.slice(1).forEach((window, index) => {
      const older = document.createElement('tr');
      older.className = row.className;
      older.classList.remove('monitor-window-row');
      older.classList.add('monitor-history-row');
      older.dataset.historyAccountId = account.id;
      older.append(...makeMonitorWindowCells(account, collecting, window, index + 2));
      rows.append(older);
    });
    return rows;
  }

  const requestTypeName = (type) => ({ sync: '同步', stream: '流式', ws_v2: 'WebSocket v2' })[type] || safeText(type, '未知');
  const tokenMinimum = (value) => Number.isInteger(value) && value > 0 ? `${value >= 1000 && value % 1000 === 0 ? `${value / 1000}K` : value.toLocaleString('en-US')}` : '—';

  function monitorWindowHistory(account) {
    const history = account.diagnostics?.sampling?.history;
    return (Array.isArray(history) ? history : []).filter((window) => window && window.requests === qualityWindowSize)
      .sort((a, b) => (Date.parse(b.window_end) - Date.parse(a.window_end) || Date.parse(b.recorded_at) - Date.parse(a.recorded_at) || 0))
      .slice(0, 3);
  }

  function monitorCurrentWindows(account) {
    const sampling = account.diagnostics?.sampling;
    return sampling ? [{ ...sampling, isCurrent: true }] : [];
  }

  function makeSamplingExclusions(window) {
    const excluded = document.createElement('small');
    excluded.className = 'monitor-filter-count';
    const small = count(window.excluded_small_requests);
    const unknown = count(window.excluded_unknown_requests);
    const total = Number.isInteger(small) && Number.isInteger(unknown) ? small + unknown : '—';
    excluded.textContent = `剔除 ${total}：小请求 ${small} / 容量未知 ${unknown}`;
    return excluded;
  }

  function makeMonitorWindowCells(account, collecting, window, position) {
    const rule = currentRule(account.group, account.platform);
    const unknownWindow = window?.isCurrent && window.window_known === false;
    const sampleCount = window ? window.requests : Number(account.calls) || 0;
    const ratio = unknownWindow ? null : window ? Number.isFinite(window.cache_ratio) ? window.cache_ratio * 100 : null : account.cache;
    const cacheCell = document.createElement('td');
    cacheCell.className = 'monitor-window-cache';
    const cache = document.createElement('div');
    cache.className = 'monitor-cache';
    const cacheValue = document.createElement('strong');
    cacheValue.textContent = !collecting ? '未采集' : Number.isFinite(ratio) && sampleCount > 0 && (window || account.complete) ? `${ratio.toFixed(1)}%` : '—';
    cache.append(cacheValue);
    if (collecting && Number.isFinite(ratio) && sampleCount > 0 && (window || account.complete)) {
      const meter = document.createElement('span');
      meter.className = `meter${account.group !== 'always_on' && ratio < rule.min_cache_ratio * 100 ? ' low' : ''}`;
      const fill = document.createElement('span');
      fill.style.width = `${Math.max(0, Math.min(100, ratio))}%`;
      meter.append(fill);
      cache.append(meter);
    } else cache.classList.add('unknown');
    cacheCell.append(cache);
    const sampleCell = document.createElement('td');
    sampleCell.className = 'monitor-sample monitor-window-sample';
    if (window) {
      const label = document.createElement('strong');
      label.className = `monitor-window-label ${['healthy', 'abnormal'].includes(window.result) ? window.result : 'unknown'}`;
      label.textContent = `${window.isCurrent ? '当前周期' : `周期 ${position}`}${window.isCurrent ? '' : ` · ${({ healthy: '健康', abnormal: '异常' })[window.result] || '未知'}`}`;
      sampleCell.append(label);
      if (!window.isCurrent) {
        const time = document.createElement('small');
        const start = new Date(window.window_start);
        const end = new Date(window.window_end);
        const dateOptions = { month: '2-digit', day: '2-digit' };
        const timeOptions = { hour: '2-digit', minute: '2-digit', hour12: false };
        const startDate = start.toLocaleDateString('zh-CN', dateOptions);
        const endDate = end.toLocaleDateString('zh-CN', dateOptions);
        time.textContent = `${startDate} ${start.toLocaleTimeString('zh-CN', timeOptions)}–${startDate === endDate ? '' : `${endDate} `}${end.toLocaleTimeString('zh-CN', timeOptions)}`;
        sampleCell.append(time);
      }
    }
    const valid = document.createElement('span');
    valid.textContent = !collecting ? '—' : unknownWindow ? '进度未知' : window ? `有效 ${sampleCount}/${qualityWindowSize} · ≥${tokenMinimum(window.min_input_tokens)}` : '等待采样';
    valid.title = '有效请求须达到输入上下文门槛；每 15 个有效请求组成一个周期。';
    sampleCell.append(valid);
    if (collecting && window && !unknownWindow) {
      sampleCell.append(makeSamplingExclusions(window));
    }
    const ttft = window ? window.average_first_token_ms : account.averageFirstTokenMs;
    const ttftSamples = window ? window.first_token_samples : account.firstTokenSamples;
    const ttftCell = document.createElement('td');
    ttftCell.className = 'monitor-sample monitor-window-ttft';
    const limit = !window && account.firstTokenLimitReached ? ' · 已达扫描上限' : '';
    ttftCell.textContent = !collecting || unknownWindow ? '—' : Number.isFinite(ttft) && ttftSamples >= qualityWindowSize
      ? `${Math.round(ttft)}ms${limit}` : `样本不足 ${ttftSamples || 0}/${qualityWindowSize}${limit}`;
    return [cacheCell, sampleCell, ttftCell];
  }

  function makeScheduleStatus(account) {
    const badge = document.createElement('span');
    const known = !resetSessionPending && account.resolved && typeof account.schedulable === 'boolean';
    badge.className = `schedule-state ${!known ? 'pending' : account.schedulable ? 'enabled' : 'disabled'}`;
    badge.textContent = !known ? '待同步' : account.schedulable ? '调度中' : '未调度';
    badge.title = !known ? '等待同步宿主调度状态' : account.schedulable ? '宿主调度已开启' : '宿主调度已关闭';
    badge.setAttribute('aria-label', `调度情况：${badge.textContent}`);
    return badge;
  }

  function makeEventAction(event) {
    const labels = { pause: '暂停调度', paused: '异常停用', priority: '同步优先级', ready: '开放调用', temporary: '临时组暂停',
      group_wait: '等待前序组', exclusive_wait: '独占等待', unavailable: '宿主不可用', needs_rate: '等待倍率', pending: '异常确认中',
      deprioritize: '降低优先级', restored: '恢复调用', manual_change: '检测人工变更', skipped: '跳过动作',
      temporary_retry: '临时异常重试', account_error: '账号异常', account_repaired: '人工修复', recovery_wait: '等待恢复',
      recovery_trial: '恢复试运行', recovering: '恢复中', recovered: '恢复通过', policy_wait: '策略等待',
      manually_disabled: '手动停用', observing: '采样观察', unknown: '状态未知' };
    const badge = document.createElement('span');
    badge.className = 'event-action';
    if (['restored', 'recovered', 'account_repaired'].includes(event.action)) badge.classList.add('restore');
    else if (['manual_change', 'account_error'].includes(event.action)) badge.classList.add('danger');
    else if (event.action === 'skipped') badge.classList.add('warning');
    badge.textContent = labels[event.action] || safeText(event.action, '记录');
    return badge;
  }

  function renderEventPage() {
    const body = $('event-rows');
    const empty = $('event-empty');
    const pagination = $('event-pagination');
    const pageLabel = $('event-page-label');
    const previous = $('event-prev');
    const next = $('event-next');
    if (!body || !empty || !pagination || !pageLabel || !previous || !next) return;
    const totalPages = Math.max(1, Math.ceil(eventLog.length / eventPageSize));
    eventPage = Math.min(Math.max(1, eventPage), totalPages);
    const pageItems = eventLog.slice((eventPage - 1) * eventPageSize, eventPage * eventPageSize);
    const rows = pageItems.map((event) => {
      const row = document.createElement('tr');
      const time = document.createElement('td');
      time.className = 'event-time';
      time.textContent = event.time ? new Date(event.time).toLocaleString('zh-CN', { hour12: false }) : '—';
      const account = document.createElement('td');
      account.className = 'event-account';
      const accountID = Number(event.account_id);
      const match = accounts.find((item) => Number(item.id) === accountID);
      const accountName = typeof event.account_name === 'string' && event.account_name.trim() ? event.account_name.trim() : match?.name || '账号';
      account.textContent = accountID > 0 ? `${accountName} · ID ${accountID}` : '系统';
      const action = document.createElement('td');
      action.append(makeEventAction(event));
      const message = document.createElement('td');
      message.className = 'event-message';
      message.textContent = safeText(event.message, '—');
      row.append(time, account, action, message);
      return row;
    });
    body.replaceChildren(...rows);
    empty.hidden = rows.length > 0;
    pagination.hidden = eventLog.length === 0;
    pageLabel.textContent = `第 ${eventPage} / ${totalPages} 页`;
    previous.disabled = eventPage <= 1;
    next.disabled = eventPage >= totalPages;
  }

  function renderEvents(events) {
    const incoming = Array.isArray(events) ? events : [];
    eventLog = incoming.filter((event) => !hiddenEventKeys.has(eventKey(event))).slice().reverse();
    renderEventPage();
    const clear = $('event-clear');
    if (clear) clear.disabled = !configLoaded || busy || eventLog.length === 0;
  }

  function eventKey(event) {
    return [event?.time || '', event?.account_id || 0, event?.action || '', event?.message || ''].join('|');
  }

  function renderAccountEvents(account) {
    const events = eventLog.filter((event) => String(event.account_id) === account.id).slice(0, 5);
    $('runtime-account-events').replaceChildren(...events.map((event) => {
      const item = document.createElement('li');
      const heading = document.createElement('div');
      const time = document.createElement('time');
      time.textContent = runtimeTime(Date.parse(event.time));
      heading.append(makeEventAction(event), time);
      const message = document.createElement('p');
      message.textContent = safeText(event.message, '—');
      item.append(heading, message);
      return item;
    }));
    $('runtime-account-events-empty').hidden = events.length > 0;
  }

  function editButton(account) {
    const button = document.createElement('button');
    button.className = 'row-edit';
    button.type = 'button';
    button.textContent = '信息修改';
    button.disabled = !configLoaded || busy || !runtimeReady || automationEnabled;
    button.addEventListener('click', () => openAccountDialog(account));
    return button;
  }

  function removeButton(account) {
    const button = document.createElement('button');
    button.className = 'row-remove';
    button.type = 'button';
    button.textContent = '删除';
    button.title = '从插件监控列表删除，不删除 Sub2API 源账号';
    button.disabled = !configLoaded || busy || !runtimeReady || automationEnabled;
    button.addEventListener('click', () => {
      if (automationEnabled) return;
      removingAccount = account;
      $('delete-account-title').textContent = `删除账号 ${account.id}`;
      $('delete-account-dialog').showModal();
    });
    return button;
  }

  function accountActions(account) {
    const actions = document.createElement('div');
    actions.className = 'account-actions';
    actions.append(scheduleButton(account), editButton(account), removeButton(account));
    return actions;
  }

  function scheduleButton(account) {
    const button = document.createElement('button');
    const enabled = account.desiredSchedulable !== false;
    button.className = `row-schedule ${enabled ? 'on' : 'off'}`;
    button.type = 'button';
    button.dataset.resolved = String(account.resolved === true);
    button.textContent = enabled ? '停止调用' : '恢复调用';
    button.setAttribute('aria-pressed', String(enabled));
    button.title = enabled ? '保留在当前组，自动管理启用后不参与该组调用' : '允许此账号参与当前组调用';
    button.disabled = !configLoaded || busy || automationEnabled || !account.resolved;
    button.addEventListener('click', async () => {
      if (automationEnabled || busy) return;
      const previous = copy(accounts);
      account.desiredSchedulable = !enabled;
      renderGroups();
      if (!await saveConfig(false)) {
        accounts = previous;
        renderGroups();
      }
    });
    return button;
  }

  function renderAccountRow(account) {
    const row = document.createElement('div');
    row.className = 'account-row';
    enableAccountDrag(row, account);
    row.append(makeIdentity(account, true), makeRatePair(account), accountActions(account));
    return row;
  }

  function showRuntimeDetails(account) {
    detailAccountId = account.id;
    renderRuntimeDetails(account);
    $('runtime-detail-dialog').showModal();
  }

  const unknownErrorCount = '宿主暂不提供，不能统计502';
  const count = (value) => Number.isInteger(value) && value >= 0 ? value : '—';
  const runtimeTime = (value) => Number.isFinite(value) ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '—';
  const duration = (seconds) => {
    const whole = Math.ceil(seconds);
    const hours = Math.floor(whole / 3600);
    const minutes = Math.floor(whole / 60) % 60;
    return `${hours ? `${hours} 小时 ` : ''}${minutes ? `${minutes} 分 ` : ''}${whole % 60 || !hours && !minutes ? `${whole % 60} 秒` : ''}`.trim();
  };

  function renderRuntimeDetails(account) {
    const state = runtimeGroupState(account);
    const status = $('runtime-detail-status');
    $('runtime-detail-title').textContent = `${account.name} · 运行详情`;
    $('runtime-detail-name').textContent = account.name;
    $('runtime-detail-meta').textContent = `ID ${account.id} · ${platformNames[account.platform] || account.platform} · API Key`;
    $('runtime-detail-avatar').textContent = account.letter || account.name.slice(0, 1).toUpperCase();
    status.textContent = state.label;
    status.className = `runtime-detail-status ${state.key}`;
    renderAccountEvents(account);
    const group = groupFor(account.group);
    const configuredPriority = groupPriority(account.group, account.platform);
    $('runtime-detail-group').textContent = `${group.name}组${group.alwaysOn ? ' · 独立并行' : ''}`;
    $('runtime-detail-host').textContent = !automationEnabled ? '未启用管理，调度由宿主控制'
      : resetSessionPending || !account.resolved ? '等待同步宿主状态'
        : account.availability || (account.state === 'account_error' ? '宿主账号状态为 error'
          : account.state === 'unavailable' ? account.reason || '宿主暂不可调用' : '未报告宿主异常');
    const rate = makeRatePair(account);
    $('runtime-detail-rate').textContent = [...rate.children].map((part) => part.textContent).join(' · ');
    $('runtime-detail-priority').textContent = `当前 ${!resetSessionPending && account.resolved && Number.isFinite(account.priority) ? account.priority : '待同步'} · 配置 ${configuredPriority}`;
    const collecting = automationEnabled && samplingActive;
    const rule = currentRule(account.group, account.platform);
    $('runtime-detail-cache').textContent = !collecting ? '未采集' : Number.isFinite(account.cache)
      ? `${account.cache.toFixed(1)}% · ${account.calls || 0} 次` : `— · ${account.calls || 0} 次`;
    const ttftLimit = account.firstTokenLimitReached ? ' · 已达 5000 条扫描上限' : '';
    $('runtime-detail-ttft').textContent = !collecting ? '未采集' : Number.isFinite(account.averageFirstTokenMs) && account.firstTokenSamples >= qualityWindowSize
      ? `${Math.round(account.averageFirstTokenMs)} ms · ${qualityWindowSize} 个有效样本${ttftLimit}` : `样本不足 · ${account.firstTokenSamples || 0} / ${qualityWindowSize}${ttftLimit}`;
    setText('runtime-detail-filter', collecting ? `≥${tokenMinimum(account.minimumInputTokens ?? rule.min_input_tokens)} Token` : '未采集');
    const reason = collecting ? account.reason || account.availability : '';
    $('runtime-detail-reason').textContent = reason || '当前状态没有异常说明。';
    const syncError = automationEnabled && !resetSessionPending ? account.syncError || '' : '';
    setText('runtime-detail-error', syncError);
    $('runtime-sync-error').hidden = !syncError;
    const recovery = $('runtime-detail-recovery');
    recovery.textContent = recoveryText(account);
    recovery.parentElement.hidden = !recovery.textContent;
    const runtime = collecting ? account.diagnostics : null;
    const checkQuality = !group.alwaysOn && group.callable !== false;
    $('runtime-diagnostics').hidden = !runtime;
    setText('runtime-detail-error-requests', runtime?.errorRequests !== null && runtime?.errorRequests !== undefined ? `${runtime.errorRequests} 次` : unknownErrorCount);
    setText('runtime-detail-status-since', runtimeTime(runtime?.statusSince));
    setText('runtime-detail-failure-kind', runtime ? ({ temporary: '宿主临时异常', quality: '质量异常', account: '账号异常', policy: '策略限制' })[runtime.failureKind] || '无' : '—');
    setText('runtime-detail-failure-counts', `${count(runtime?.failureCount)} / ${count(runtime?.temporaryFailures)}`);
    setText('runtime-detail-recovery-counts', `${count(runtime?.recoveryAttempts)} / ${count(runtime?.recoveryFailures)}`);
    setText('runtime-detail-last-failure', runtimeTime(runtime?.lastFailureAt));
    setText('runtime-detail-last-attempt', runtimeTime(runtime?.lastRecoveryAttemptAt));
    setText('runtime-detail-degradation', runtime && Number.isInteger(runtime.tier) ? `${runtime.tier} 级 · 仅影响重试间隔，不改组或优先级` : '—');
    setText('runtime-detail-recovery-start', runtimeTime(runtime?.recoveryStartedAt));
    setText('runtime-detail-recovery-deadline', runtimeTime(runtime?.recoveryDeadline));
    setText('runtime-detail-quality-windows', !runtime ? '—' : checkQuality
      ? `异常 ${count(runtime.badWindows)} / ${confirmWindows(rule.trigger_samples)}（至少 ${duration(rule.confirmation_seconds)}） · 健康 ${count(runtime.goodWindows)} / ${confirmWindows(rule.recovery_samples)}（至少试运行 ${duration(rule.recovery_seconds)}）`
      : group.alwaysOn ? '常开组不执行质量规则' : '临时组不参与调用');
    renderQualityWindows(account, collecting && group.callable !== false, rule, checkQuality);
    updateDetailRetry(account);
  }

  function renderQualityWindows(account, collecting, rule, checkQuality) {
    const section = $('runtime-quality');
    const sampled = collecting ? account : {};
    const history = monitorWindowHistory(sampled);
    const current = monitorCurrentWindows(sampled);
    const scanLimitReached = sampled.diagnostics?.scanLimitReached === true;
    section.hidden = history.length === 0 && current.length === 0 && !scanLimitReached;
    $('runtime-quality-scan-limit').hidden = !scanLimitReached;
    $('runtime-quality-windows').replaceChildren(...history.map(makeRuntimeWindow));
    $('runtime-quality-empty').hidden = history.length > 0;
    $('runtime-quality-current').hidden = current.length === 0;
    const sampling = current[0];
    setText('runtime-quality-current-summary', sampling ? `当前采样 · ${sampling.window_known === false ? '进度未知' : `${count(sampling.requests)}/${qualityWindowSize}`}` : '当前采样');
    $('runtime-quality-current-windows').replaceChildren(...current.map((window, index) => {
      const entry = makeRuntimeWindow(window, index);
      if (checkQuality) {
        const progress = document.createElement('p');
        progress.textContent = `连续异常 ${count(window.bad_windows)}/${confirmWindows(rule.trigger_samples)} · 连续健康 ${count(window.good_windows)}/${confirmWindows(rule.recovery_samples)}`;
        const evaluated = document.createElement('p');
        evaluated.textContent = `最近评估 ${runtimeTime(Date.parse(window.last_evaluated_at))}`;
        entry.append(progress, evaluated);
      }
      return entry;
    }));
  }

  function makeRuntimeWindow(window, index) {
    const entry = document.createElement('li');
    const heading = document.createElement('strong');
    const unknown = window.isCurrent && window.window_known === false;
    const result = window.isCurrent ? unknown ? '未知' : '采样中' : ({ healthy: '健康', abnormal: '异常' })[window.result] || '未知';
    heading.className = `monitor-window-label ${['healthy', 'abnormal'].includes(window.result) ? window.result : 'unknown'}`;
    heading.textContent = `${window.isCurrent ? '当前窗口' : `窗口 ${index + 1}`} · ${result}`;
    entry.append(heading);
    if (!window.isCurrent) {
      const time = document.createElement('p');
      time.textContent = `${runtimeTime(Date.parse(window.window_start))} → ${runtimeTime(Date.parse(window.window_end))}`;
      entry.append(time);
    }
    const progress = document.createElement('p');
    progress.textContent = `${unknown ? '进度与剔除数未知' : `有效 ${count(window.requests)}/${qualityWindowSize}`} · ≥${tokenMinimum(window.min_input_tokens)}`;
    entry.append(progress);
    if (unknown) {
      const note = document.createElement('p');
      note.textContent = '未完整扫描至封窗边界';
      entry.append(note);
    } else {
      const metrics = document.createElement('p');
      const ratio = Number.isFinite(window.cache_ratio) ? `${(window.cache_ratio * 100).toFixed(1)}%` : '—';
      const ttft = Number.isFinite(window.average_first_token_ms) && window.first_token_samples >= qualityWindowSize
        ? `${Math.round(window.average_first_token_ms)} ms` : `样本不足 ${count(window.first_token_samples)}/${qualityWindowSize}`;
      metrics.textContent = `缓存 ${ratio} · 首字 ${ttft}`;
      entry.append(metrics, makeSamplingExclusions(window));
      const models = makeWindowModels(window);
      if (models) entry.append(models);
    }
    if (window.reason) {
      const reason = document.createElement('p');
      reason.textContent = safeText(window.reason, '');
      entry.append(reason);
    }
    return entry;
  }

  function makeWindowModels(window) {
    const models = Array.isArray(window.models) ? window.models : [];
    if (!models.length) return null;
    const usage = document.createElement('div');
    usage.className = 'runtime-window-models';
    const heading = document.createElement('p');
    heading.textContent = '模型调用';
    usage.append(heading);
    for (const model of models) {
      const row = document.createElement('div');
      row.className = 'runtime-window-model';
      const label = document.createElement('p');
      label.textContent = `${safeText(model.model, '未知模型')} · ${requestTypeName(model.request_type)} · ${count(model.requests)} 次有效`;
      const metrics = document.createElement('p');
      const ratio = Number.isFinite(model.cache_ratio) ? `${(model.cache_ratio * 100).toFixed(1)}%` : '—';
      const ttft = Number.isFinite(model.average_first_token_ms) && model.first_token_samples > 0
        ? `${Math.round(model.average_first_token_ms)} ms（${count(model.first_token_samples)}/${count(model.requests)}）`
        : `均值未知（${count(model.first_token_samples)}/${count(model.requests)}）`;
      metrics.textContent = `缓存 ${ratio} · 首字 ${ttft}`;
      row.append(label, metrics);
      usage.append(row);
    }
    return usage;
  }

  function updateDetailRetry(account) {
    const runtime = automationEnabled && samplingActive ? account?.diagnostics : null;
    let text = '—';
    if (Number.isFinite(runtime?.nextRetryAt)) {
      const seconds = (runtime.nextRetryAt - Date.now()) / 1000;
      const start = Math.max(...[runtime.lastFailureAt, runtime.lastRecoveryAttemptAt, runtime.statusSince]
        .filter((at) => Number.isFinite(at) && at < runtime.nextRetryAt));
      const interval = Number.isFinite(start) && account.state !== 'recovery_wait'
        ? ` · 本次等待 ${duration((runtime.nextRetryAt - start) / 1000)}` : '';
      text = `${runtimeTime(runtime.nextRetryAt)} · ${seconds > 0 ? `还剩 ${duration(seconds)}` : '已到期'}${interval}`;
    }
    setText('runtime-detail-next-retry', text);
  }

  function updateMonitorRetry(countdown) {
    const seconds = (Number(countdown.dataset.retryAt) - Date.now()) / 1000;
    countdown.textContent = seconds > 0 ? `重试剩余 ${duration(seconds)}` : '重试已到期';
    const status = countdown.parentElement;
    const label = status.querySelector('.monitor-status-label').textContent;
    status.title = `${label} · ${countdown.textContent} · 点击查看运行详情`;
    const accountName = status.closest('tr').querySelector('.monitor-identity strong').textContent;
    status.setAttribute('aria-label', `${accountName}：${label}，${countdown.textContent}，查看运行详情`);
  }

  function recoveryText(account) {
    if (!automationEnabled) return '';
    if (account.state === 'account_error') return '需人工检查账号凭证或配置；插件不会自动清除宿主账号错误。';
    if (account.state === 'manual_change') return `当前调度：${account.schedulable ? '开启' : '关闭'} · 本轮由人工控制，插件不自动修改`;
    if (account.group === 'temporary') return `当前调度：${account.schedulable ? '开启' : '关闭'} · 临时组不参与自动调用`;
    if (account.state === 'recovery_wait') return '重试已到期，正在等待前序普通组；暂不开始试运行。';
    if (account.diagnostics?.trial) {
      return `按原组配置优先级 ${groupPriority(account.group, account.platform)} 试运行；较低兜底组保持开启，确认健康后才退出兜底。`;
    }
    if (account.state === 'temporary_retry') return '临时异常退避中；到期后自动重试。';
    if (account.state === 'paused') return '质量异常退避中；到期且前序组不可用时按原组优先级试运行。';
    return '';
  }

  function renderExceptionArea(exceptions, collecting) {
    const details = document.createElement('section');
    details.className = 'exception-area';
    const summary = document.createElement('h4');
    summary.innerHTML = `不可调用区 <span class="exception-count">${exceptions.length}</span>`;
    details.append(summary);
    if (!exceptions.length) {
      const empty = document.createElement('p');
      empty.className = 'monitor-group-empty';
      empty.textContent = '暂无不可调用账号';
      details.append(empty);
    }
    if (exceptions.length) {
      const table = $('monitor-table-template').content.cloneNode(true);
      table.querySelector('table').setAttribute('aria-label', '不可调用账号监控');
      table.querySelector('tbody').append(...exceptions.map((account) => renderMonitorRow(account, collecting, true)));
      details.append(table);
    }
    return details;
  }

  function renderGroup(group, index) {
    const section = document.createElement('article');
    section.className = 'group-section';
    section.dataset.groupId = group.id;
    section.style.setProperty('--group-color', group.color);
    section.style.setProperty('--group-soft', group.soft);
    const manageable = groupAccounts(group.id);
    const header = document.createElement('div');
    header.className = 'group-header';

    const title = document.createElement('div');
    title.className = 'group-title';
    const order = document.createElement('span');
    order.className = 'group-order-number';
    order.textContent = String(index + 1).padStart(2, '0');
    const titleText = document.createElement('div');
    const heading = document.createElement('h3');
    heading.textContent = group.name;
    const note = document.createElement('p');
    note.textContent = group.callable === false ? '不参与调用 · 自动管理时关闭调度' : group.alwaysOn ? '持续开启 · 宿主临时状态自动恢复' : '前序组无可用账号后调用';
    titleText.append(heading, note);
    title.append(order, titleText);

    const priority = document.createElement('span');
    priority.className = 'group-badge';
    priority.textContent = `优先级 ${groupPriority(group.id)}`;

    const availability = document.createElement('div');
    availability.className = 'group-availability';
    availability.textContent = `${manageable.length} 个账号`;

    const actions = document.createElement('div');
    actions.className = 'group-actions';
    const expand = document.createElement('button');
    expand.className = 'expand-button';
    expand.type = 'button';
    expand.dataset.expanded = 'true';
    expand.setAttribute('aria-label', `折叠${group.name}组`);
    actions.append(expand);
    header.append(title, priority, availability, actions);

    const content = document.createElement('div');
    content.className = 'group-content';
    content.id = `group-content-${group.id}`;
    content.hidden = collapsedGroups.has(group.id);
    expand.dataset.expanded = String(!content.hidden);
    expand.setAttribute('aria-controls', content.id);
    expand.setAttribute('aria-expanded', String(!content.hidden));
    expand.setAttribute('aria-label', `${content.hidden ? '展开' : '折叠'}${group.name}组`);
    for (const account of manageable) content.append(renderAccountRow(account));
    if (manageable.length === 0) {
      const empty = document.createElement('p');
      empty.className = 'empty-group';
      empty.textContent = group.callable === false ? '新添加的账号默认放在这里；拖入调用组后才参与调用。' : '当前组暂无账号。';
      content.append(empty);
    }
    expand.addEventListener('click', () => {
      content.hidden = !content.hidden;
      if (content.hidden) collapsedGroups.add(group.id);
      else collapsedGroups.delete(group.id);
      expand.dataset.expanded = String(!content.hidden);
      expand.setAttribute('aria-expanded', String(!content.hidden));
      expand.setAttribute('aria-label', `${content.hidden ? '展开' : '折叠'}${group.name}组`);
    });
    section.append(header, content);
    const summary = document.createElement('p');
    summary.className = 'group-settings-summary';
    const rule = currentRule(group.id);
    summary.textContent = group.callable === false ? '不参与调用 · 自动管理时关闭调度 · 优先级 99'
      : group.alwaysOn ? `独立并行 · 重试基准 ${rule.cooldown_seconds} 秒 · 优先级 ${groupPriority(group.id)}`
        : `缓存下限 ${rule.min_cache_ratio > 0 ? `${Number((rule.min_cache_ratio * 100).toFixed(1))}%` : '未启用'} · 首字上限 ${rule.max_average_first_token_ms > 0 ? `${rule.max_average_first_token_ms} ms` : '未启用'} · 连续 ${confirmWindows(rule.trigger_samples)} 个不重叠窗口 · 健康 ${confirmWindows(rule.recovery_samples)} 窗口恢复 · 重试基准 ${rule.cooldown_seconds} 秒${group.special && rule.exclusive_rate_below > 0 ? ` · 独占倍率 < ${rule.exclusive_rate_below}× · 独占缓存 ≥ ${Number(((rule.exclusive_min_cache_ratio || .8) * 100).toFixed(1))}% · 独占首字 ≤ ${rule.exclusive_max_average_first_token_ms || 10000} ms` : ''}`;
    header.after(summary);
    section.addEventListener('dragover', (event) => {
      if (draggedAccountId && accounts.some((account) => account.id === draggedAccountId && account.group !== group.id)) {
        event.preventDefault();
        section.classList.add('drag-over');
      }
    });
    section.addEventListener('dragleave', () => section.classList.remove('drag-over'));
    section.addEventListener('drop', async (event) => {
      if (!draggedAccountId) return;
      event.preventDefault();
      section.classList.remove('drag-over');
      const account = accounts.find((item) => item.id === draggedAccountId);
      if (account && !busy && configLoaded && !automationEnabled && account.group !== group.id) {
        const oldGroup = account.group;
        account.group = group.id;
        if (!await saveConfig(false)) account.group = oldGroup;
        renderGroups();
      }
    });
    return section;
  }

  function enableAccountDrag(row, account) {
    row.draggable = configLoaded && !busy && !automationEnabled;
    row.dataset.accountId = account.id;
    row.addEventListener('dragstart', (event) => {
      draggedAccountId = account.id;
      event.dataTransfer.effectAllowed = 'move';
      event.dataTransfer.setData('text/plain', account.id);
      row.classList.add('dragging');
    });
    row.addEventListener('dragend', () => {
      draggedAccountId = null;
      row.classList.remove('dragging');
      document.querySelectorAll('.group-section.drag-over').forEach((section) => section.classList.remove('drag-over'));
    });
  }

  function renderGroups() {
    renderPlatformOptions();
    const list = $('groups-list');
    list.replaceChildren(...sortedGroups().map(renderGroup));
    $('account-count').textContent = String(visibleAccounts().length);
    $('runtime-account-count').textContent = String(accounts.length);
    renderMonitorGroups();
    renderFlowSteps();
    setText('runtime-platform', platformNames[activePlatform] || activePlatform);
    if ($('runtime-detail-dialog').open) {
      const account = accounts.find((item) => item.id === detailAccountId);
      if (account) renderRuntimeDetails(account);
      else $('runtime-detail-dialog').close();
    }
    updateAutomationGate();
  }

  function isCallable(account) {
    return automationEnabled && account.group !== 'temporary' && account.schedulable &&
      (['ready', 'sampling', 'pending'].includes(account.state) || account.diagnostics?.trial === true && account.state === 'recovering');
  }

  function runtimeGroupState(account) {
    if (account.state === 'unsupported') return { key: 'unsupported', label: '不支持的账号' };
    if (!automationEnabled) return account.group === 'temporary' ? { key: 'inactive', label: '待定不调用' }
      : account.desiredSchedulable === false ? { key: 'stopped', label: '组内停止调用' } : { key: 'monitor', label: '未启用管理' };
    if (account.state === 'manual_change') return { key: 'manual', label: `手动操作 · 调度${account.schedulable ? '开启' : '关闭'}` };
    if (account.state === 'account_error') return { key: 'account-error', label: '账号异常 · 需人工处理' };
    if (account.state === 'temporary_retry') return { key: 'retrying', label: '临时异常 · 退避重试' };
    if (account.state === 'recovery_wait') return { key: 'waiting', label: '重试到期 · 等待前序组' };
    if (account.state === 'unavailable') return { key: 'unavailable', label: '宿主不可调用' };
    if (account.group === 'temporary') return { key: 'inactive', label: '待定不调用' };
    if (account.desiredSchedulable === false) return { key: 'stopped', label: '组内停止调用' };
    if (account.status === 'exception' || account.state === 'paused' || account.state === 'deprioritized') return { key: 'exception', label: '规则异常停用' };
    if (account.state === 'recovering') return { key: 'recovering', label: account.diagnostics?.trial ? '原组恢复试运行' : '恢复宿主设置' };
    if (account.state === 'pending') return { key: 'pending', label: account.schedulable ? '异常确认中 · 可调用' : '异常确认中 · 未开放' };
    if (account.state === 'needs_rate') return { key: 'needs-rate', label: '倍率不可用' };
    if (account.state === 'sampling') return { key: 'sampling', label: account.schedulable ? '等待采样 · 可调用' : '等待采样 · 未开放' };
    if (account.state === 'observing') return { key: 'sampling', label: '等待本轮采样' };
    if (account.schedulable && account.state === 'ready') return { key: 'calling', label: '可调用' };
    if (account.state === 'group_wait') return { key: 'waiting', label: '等待前序组' };
    if (account.state === 'exclusive_wait') return { key: 'waiting', label: '独占等待' };
    if (account.state === 'ready') return { key: 'waiting', label: '等待调用' };
    return { key: 'waiting', label: '等待采集' };
  }

  function renderAccountOverview(visible) {
    const exceptions = visible.filter(isUnavailable).length;
    const needsRate = visible.filter((account) => account.status !== 'exception' && effectiveRate(account) === null).length;
    const callable = visible.filter(isCallable).length;
    $('overview-total').textContent = String(visible.length);
    $('overview-callable').textContent = String(callable);
    $('overview-exception').textContent = String(exceptions);
    $('overview-needs-rate').textContent = String(needsRate);
  }

  function renderRouteOrder(monitoredGroups) {
    const route = $('route-order');
    route.parentElement.hidden = monitoredGroups.length === 0;
    route.replaceChildren(...monitoredGroups.map(({ group, rank }) => {
      const item = document.createElement('span');
      item.className = group.alwaysOn ? 'route-item parallel' : 'route-item';
      item.style.color = group.color;
      item.innerHTML = group.alwaysOn ? '常开 · 并行' : `<span class="route-rank">${rank}</span>${group.name}`;
      return item;
    }));
  }

  function renderFlowSteps() {
    const target = $('flow-steps');
    target.replaceChildren();
    callGroups.forEach((group) => {
      const step = document.createElement('span');
      step.className = group.alwaysOn ? 'flow-step parallel' : 'flow-step';
      step.style.color = group.color;
      step.innerHTML = `${group.name}${group.alwaysOn ? ' · 并行' : ''}<span>P${groupPriority(group.id)}</span>`;
      target.append(step);
    });
  }

  function updateAutomationGate() {
    const missing = accounts.filter((account) => account.group !== 'temporary' && effectiveRate(account) === null).length;
    const managementDisabled = !configLoaded || busy || !runtimeReady;
    const settingsDisabled = managementDisabled || automationEnabled;
    const button = $('automation-toggle');
    button.disabled = managementDisabled || (!automationEnabled && (accounts.length === 0 || missing > 0));
    button.textContent = automationEnabled ? '停止监控' : '启动监控';
    button.classList.toggle('is-on', automationEnabled);
    button.classList.toggle('is-off', !automationEnabled);
    button.title = automationEnabled ? '自动管理启用时参数锁定；关闭并等待恢复后再修改' : missing ? `还有 ${missing} 个非临时组账号需要有效倍率` : accounts.length ? '启用自动管理并同步组优先级' : '先添加监控账号';
    $('automation-status').textContent = automationEnabled ? '自动管理已启用' : missing ? `${missing} 个账号待设置倍率` : '自动管理未启用';
    $('runtime-automation-state').textContent = automationEnabled ? '已启用' : '未启用';
    $('new-account-id').disabled = settingsDisabled;
    $('add-account-button').disabled = settingsDisabled;
    for (const control of document.querySelectorAll('#view-rules input, #save-rules, #reset-rules, #sort-direction, #edit-group, input[name="rate_mode"], #edit-fixed-rate, #edit-save-button, #delete-account-confirm, .row-edit, .row-remove')) control.disabled = settingsDisabled;
    for (const control of document.querySelectorAll('#view-connection input, #save-config, #test-connection')) control.disabled = settingsDisabled;
    for (const control of document.querySelectorAll('.row-schedule')) control.disabled = managementDisabled || automationEnabled || control.dataset.resolved !== 'true';
    updateRuleInputs();
    updatePollControls();
  }

  function openAccountDialog(account) {
    if (automationEnabled) return;
    editingAccount = account;
    $('dialog-title').textContent = `编辑账号 ${account.id}`;
    $('dialog-name').textContent = account.name;
    $('dialog-meta').textContent = `ID ${account.id} · ${platformNames[account.platform] || account.platform} · API Key`;
    $('dialog-avatar').textContent = account.letter || account.name.slice(0, 1).toUpperCase();
    $('dialog-status').textContent = accountState(account);
    const hasUpstream = Number.isFinite(account.upstreamRate);
    $('dialog-upstream').textContent = hasUpstream ? `当前上游倍率：${number(account.upstreamRate)}`
      : '上游不支持或尚未返回有效倍率时，使用默认 1.0×';
    $('edit-group').replaceChildren(...groups.map((group) => new Option(`${group.name} · 优先级 ${groupPriority(group.id)}`, group.id)));
    $('edit-group').value = account.group;
    const mode = account.rateMode === 'fixed' ? 'fixed' : 'upstream';
    const upstreamChoice = document.querySelector('input[name="rate_mode"][value="upstream"]');
    upstreamChoice.disabled = false;
    document.querySelector(`input[name="rate_mode"][value="${mode}"]`).checked = true;
    $('edit-fixed-rate').value = Number.isFinite(account.fixedRate) ? account.fixedRate : 1;
    $('edit-fixed-rate').disabled = mode !== 'fixed';
    $('edit-rate-error').hidden = true;
    $('account-dialog').showModal();
  }

  function addAccount(id) {
    if (automationEnabled) return '请先关闭自动管理并等待恢复后再添加账号。';
    if (!/^[1-9]\d*$/.test(id) || !Number.isSafeInteger(Number(id))) return '账号 ID 需为有效正整数。';
    if (accounts.some((account) => account.id === id)) return `账号 ${id} 已在监控列表中。`;
    accounts.push({ id, name: `监控账号 ${id}`, platform: 'unknown', group: 'temporary', status: 'ready', fixedRate: 1, accountRate: null, upstreamRate: null, cache: null, calls: 0, letter: id.slice(-2), rateMode: 'upstream', desiredSchedulable: null, resolved: false });
    activePlatform = 'unknown';
    platformSelected = false;
    renderGroups();
    return '';
  }

  function normalizeNewAccount(account) {
    if (!account || account.resolved) return account;
    return { ...account, rateMode: 'fixed', fixedRate: Number.isFinite(account.fixedRate) ? account.fixedRate : 1 };
  }

  function copy(value) { return JSON.parse(JSON.stringify(value)); }
  function isObject(value) { return value !== null && typeof value === 'object' && !Array.isArray(value); }
  function safeText(value, fallback = '未知') {
    if (typeof value !== 'string' || !value.trim()) return fallback;
    let result = value;
    for (const secret of [...secrets].sort((a, b) => b.length - a.length)) result = result.split(secret).join('[密钥已隐藏]');
    return result.replace(/Bearer\s+[A-Za-z0-9._~+\/-]+=*/gi, 'Bearer [已隐藏]').slice(0, 1500);
  }
  function showMessage(message, kind = '') {
    const target = $('action-message');
    target.textContent = message;
    target.className = `inline-error ${kind}`.trim();
    target.hidden = !message;
  }
  function notify(type, extra = {}) {
    if (!available || disposed) return;
    window.parent.postMessage({ ...extra, source: 'sub2api-plugin-ui', bridge_token: token, type }, '*');
  }
  function request(type, extra = {}) {
    if (!available || disposed) return Promise.reject(new Error('未连接宿主，请从插件配置入口打开此页。'));
    const requestID = `account-ui-${Date.now()}-${++sequence}`;
    return new Promise((resolve, reject) => {
      const timer = window.setTimeout(() => { pending.delete(requestID); reject(new Error('宿主请求超时，请重试。')); }, type === 'plugin.status' || type === 'config.load' ? 20000 : 120000);
      pending.set(requestID, { resolve, reject, timer, type });
      notify(type, { ...extra, request_id: requestID });
    });
  }
  function receiveMessage(event) {
    const message = event.data;
    if (disposed || event.source !== window.parent || !isObject(message) || message.source !== 'sub2api-plugin-host' || message.bridge_token !== token || typeof message.request_id !== 'string') return;
    const entry = pending.get(message.request_id);
    if (!entry || message.type !== `${entry.type}.result`) return;
    pending.delete(message.request_id);
    window.clearTimeout(entry.timer);
    if (message.ok === true) entry.resolve(message);
    else {
      const detail = typeof message.error === 'string' && message.error.trim() ? safeText(message.error.trim()) : '';
      entry.reject(new Error(detail || (entry.type === 'config.test' ? '连接诊断未通过，请核对管理地址、密钥和权限。' : '宿主未完成请求，请稍后重试。')));
    }
  }
  function normalizeConfig(value) {
    const config = copy(defaults);
    if (!isObject(value)) return config;
    for (const key of Object.keys(config)) {
      if (key === 'rule' || value[key] === undefined) continue;
      config[key] = Array.isArray(config[key]) ? (Array.isArray(value[key]) ? value[key].slice() : []) : value[key];
    }
    config.upstream_rates_enabled = true;
    if (isObject(value.rule)) Object.assign(config.rule, value.rule);
    config.group_priorities = { ...defaults.group_priorities, ...(isObject(value.group_priorities) ? value.group_priorities : {}), temporary: 99 };
    config.group_rules = Object.fromEntries(groups.map((group) => [group.id, { ...defaultRule(group.id),
      ...(isObject(value.group_rules) ? value.group_rules[group.id] : config.rule), max_rate_multiplier: value.group_rules?.[group.id]?.max_rate_multiplier ?? config.rule.max_rate_multiplier ?? .2 }]));
    config.platform_group_priorities = isObject(value.platform_group_priorities) ? copy(value.platform_group_priorities) : {};
    config.platform_group_rules = isObject(value.platform_group_rules) ? copy(value.platform_group_rules) : {};
    return config;
  }
  function setField(id, value) {
    const field = $(id);
    if (!field) return;
    if (field.type === 'checkbox') field.checked = Boolean(value);
    else field.value = value == null ? '' : String(value);
  }
  function updateAddAccountControls() {
    updateAutomationGate();
    if ($('event-clear')) $('event-clear').disabled = !configLoaded || busy || eventLog.length === 0;
  }
  function clearAccountSession(account) {
    return { ...account, calls: 0, cache: null, complete: false, firstTokenSamples: 0,
      minimumInputTokens: null, excludedSmall: null, excludedUnknown: null,
      averageFirstTokenMs: null, firstTokenLimitReached: false, diagnostics: null,
      state: account.state === 'unsupported' ? 'unsupported' : 'observing', status: 'ready',
      reason: '', availability: '', syncError: '', schedulable: null, priority: null };
  }
  function applyConfig(value) {
    const config = normalizeConfig(value);
    if (savedConfig?.automation_enabled !== config.automation_enabled) {
      resetSessionPending = configLoaded && config.automation_enabled === true;
      samplingRevision++;
      samplingActive = false;
      runtimePolling = false;
      nextPollAt = null;
      accounts = accounts.map(clearAccountSession);
    }
    savedConfig = copy(config);
    if (config.admin_api_key) secrets.add(config.admin_api_key);
    setField('admin-url', config.admin_url);
    setField('admin-api-key', config.admin_api_key);
    setField('account-ids', config.account_ids.join(', '));
    setField('poll-seconds', config.poll_seconds);
    setField('window-minutes', config.window_minutes);
    setField('timeout-seconds', config.timeout_seconds);
    setField('collect-cache', true);
    setField('recovery-samples', config.rule.recovery_samples);
    setField('cooldown-seconds', config.rule.cooldown_seconds);
    setField('action', config.rule.action);
    setField('priority-penalty', config.rule.priority_penalty);
    setField('rate-source', config.rule.rate_source);
    setField('automation-enabled', config.automation_enabled);
    baseGroupPriorities = copy(config.group_priorities);
    platformGroupPriorities = copy(config.platform_group_priorities);
    groupRules = copy(config.group_rules);
    platformGroupRules = copy(config.platform_group_rules);
    automationEnabled = Boolean(config.automation_enabled);
    const settingByID = new Map((Array.isArray(config.account_settings) ? config.account_settings : [])
      .filter((setting) => setting && setting.account_id != null).map((setting) => [String(setting.account_id), setting]));
    const configured = new Set(config.account_ids.map(String));
    accounts = accounts.filter((account) => configured.has(account.id));
    for (const id of config.account_ids) {
      const key = String(id);
      const setting = settingByID.get(key);
      const group = groupFor(setting?.group)?.id || 'temporary';
    const rateMode = setting?.rate_mode === 'fixed' ? 'fixed' : 'upstream';
      const fixedRate = typeof setting?.fixed_rate === 'number' ? setting.fixed_rate : null;
      const desiredSchedulable = typeof setting?.desired_schedulable === 'boolean' ? setting.desired_schedulable : null;
      if (!accounts.some((account) => account.id === key)) accounts.push({ id: key, name: `账号 ${id}`, platform: setting?.platform || 'unknown', group, status: 'ready', state: 'unknown', fixedRate: fixedRate ?? 1, accountRate: null, upstreamRate: null, cache: null, calls: 0, letter: key.slice(-2), rateMode, desiredSchedulable });
      else {
        const account = accounts.find((item) => item.id === key);
        account.group = group;
        account.rateMode = rateMode;
        account.fixedRate = fixedRate ?? 1;
        account.desiredSchedulable = desiredSchedulable;
      }
    }
    configLoaded = true;
    renderGroups();
    renderRuleEditor();
    updateAddAccountControls();
    $('connection-message').textContent = config.admin_url ? '连接配置已加载。' : '尚未配置管理连接。';
    updateAutomationGate();
  }
  function readConfig(rulePlatform = null) {
    captureRuleEditor();
    for (const id of ['poll-seconds', 'timeout-seconds']) {
      if (!$(id).value.trim() || !$(id).checkValidity()) { $(id).reportValidity(); throw new Error('请填写有效的采集间隔和请求超时。'); }
    }
    const ids = accounts.map((account) => Number(account.id)).filter((id) => Number.isSafeInteger(id) && id > 0);
    const invalid = accounts.filter((account) => account.group !== 'temporary' && account.rateMode === 'fixed' && !Number.isFinite(account.fixedRate));
    if (invalid.length) throw new Error(`以下账号进入调用组前必须填写固定倍率：${invalid.map((account) => account.id).join('、')}`);
    return {
      admin_url: $('admin-url').value.trim(), admin_api_key: $('admin-api-key').value.trim(), account_ids: ids,
      poll_seconds: Number($('poll-seconds').value) || 60, window_minutes: Number($('window-minutes').value) || 15,
      timeout_seconds: Number($('timeout-seconds').value) || 15, upstream_rates_enabled: true,
      automation_enabled: automationEnabled, executor_hostname: savedConfig?.executor_hostname || runtimeHostname || '',
      account_settings: accounts.map((account) => ({ account_id: Number(account.id), group: groupFor(account.group)?.id || 'temporary',
        platform: account.platform === 'unknown' ? '' : account.platform,
        rate_mode: account.rateMode === 'fixed' ? 'fixed' : 'upstream',
        ...(typeof account.desiredSchedulable === 'boolean' ? { desired_schedulable: account.desiredSchedulable } : {}),
        ...(Number.isFinite(account.fixedRate) ? { fixed_rate: account.fixedRate } : {}) })),
      group_priorities: copy(baseGroupPriorities), group_rules: copy(groupRules),
      platform_group_priorities: copy(rulePlatform ? { ...savedConfig.platform_group_priorities, [rulePlatform]: platformGroupPriorities[rulePlatform] } : platformGroupPriorities),
      platform_group_rules: copy(rulePlatform ? { ...savedConfig.platform_group_rules, [rulePlatform]: platformGroupRules[rulePlatform] } : platformGroupRules),
      rule: { ...defaults.rule, ...(savedConfig?.rule || {}) },
    };
  }
  async function saveConfig(test = false, rulePlatform = null) {
    if (!configLoaded || busy || disposed) return false;
    try {
      const config = readConfig(rulePlatform);
      const drafts = rulePlatform ? { rules: copy(platformGroupRules), priorities: copy(platformGroupPriorities) } : null;
      if (config.automation_enabled && (!config.admin_url || !config.admin_api_key || !config.executor_hostname)) throw new Error('请先保存管理连接并等待运行状态就绪。');
      busy = true;
      updateAddAccountControls();
      const response = await request('config.save', { config });
      applyConfig(response.config);
      if (drafts) {
        platformGroupRules = { ...drafts.rules, [rulePlatform]: platformGroupRules[rulePlatform] };
        platformGroupPriorities = { ...drafts.priorities, [rulePlatform]: platformGroupPriorities[rulePlatform] };
        renderRuleEditor();
      }
      showMessage(test ? '配置已保存，正在测试管理连接。' : !config.admin_url && config.account_ids.length
        ? '账号 ID 已保存。请配置管理连接以读取账号信息。' : '配置已保存，后台将按新设置采集。', 'success');
      if (test) {
        const diagnostic = await request('config.test');
        if (!diagnostic.result || diagnostic.result.success !== true) throw new Error('连接诊断未通过，请检查管理 API 权限。');
        $('connection-message').textContent = '连接诊断通过。';
      }
      return true;
    } catch (error) { showMessage(error.message || '保存失败，请稍后重试。', 'error'); return false; }
    finally {
      busy = false;
      updateAddAccountControls();
      if (!disposed) { renderGroups(); refreshStatus(); }
    }
  }
  function normalizeAccount(raw) {
    const id = String(raw.id);
    const previous = accounts.find((account) => account.id === id);
    const upstream = Number.isFinite(raw.upstream_rate) ? raw.upstream_rate : null;
    const local = Number.isFinite(raw.account_rate) ? raw.account_rate : null;
    const setting = savedConfig?.account_settings?.find((item) => String(item.account_id) === id);
    const hasSetting = Boolean(setting);
    const mode = hasSetting ? setting.rate_mode === 'fixed' ? 'fixed' : 'upstream' : previous?.rateMode === 'fixed' ? 'fixed' : 'upstream';
    const fixedRate = hasSetting ? Number.isFinite(setting.fixed_rate) ? setting.fixed_rate : 1 : previous?.fixedRate ?? 1;
    const desiredSchedulable = hasSetting ? setting.desired_schedulable ?? null
      : raw.desired_schedulable ?? previous?.desiredSchedulable ?? null;
    const shownMode = mode === 'upstream' && ['default', 'waiting'].includes(raw.rate_mode) ? raw.rate_mode
      : hasSetting ? mode : ['fixed', 'upstream', 'default', 'waiting'].includes(raw.rate_mode) ? raw.rate_mode : mode;
    const shownFixed = hasSetting ? fixedRate : Number.isFinite(raw.fixed_rate) ? raw.fixed_rate : fixedRate;
    const state = samplingActive || raw.state === 'unsupported' ? raw.state : 'observing';
    return { id, name: safeText(raw.name, `账号 ${id}`), platform: raw.platform || previous?.platform || setting?.platform || 'unknown', type: raw.type,
      availability: samplingActive ? safeText(raw.availability_reason, '') : '',
      group: groupFor(setting?.group)?.id || previous?.group || 'temporary', status: ['paused', 'deprioritized', 'manual_change'].includes(state) ? 'exception' : 'ready',
      fixedRate: shownFixed, desiredSchedulable, resolved: raw.read_ok === true,
      accountRate: local, upstreamRate: upstream, cache: samplingActive && Number.isFinite(raw.cache_ratio) ? raw.cache_ratio * 100 : null,
      calls: samplingActive ? Number(raw.requests) || 0 : 0, complete: samplingActive && raw.complete === true, letter: id.slice(-2),
      minimumInputTokens: samplingActive && Number.isInteger(raw.min_input_tokens) ? raw.min_input_tokens : null,
      excludedSmall: samplingActive && Number.isInteger(raw.excluded_small_requests) && raw.excluded_small_requests >= 0 ? raw.excluded_small_requests : null,
      excludedUnknown: samplingActive && Number.isInteger(raw.excluded_unknown_requests) && raw.excluded_unknown_requests >= 0 ? raw.excluded_unknown_requests : null,
      rateMode: shownMode, reason: samplingActive ? safeText(raw.reason, '') : '',
      averageFirstTokenMs: samplingActive && Number.isFinite(raw.average_first_token_ms) ? raw.average_first_token_ms : null, firstTokenSamples: samplingActive ? Number(raw.first_token_samples) || 0 : 0,
      firstTokenLimitReached: samplingActive && raw.first_token_limit_reached === true,
      state, schedulable: raw.schedulable, priority: Number.isFinite(raw.priority) ? raw.priority : null,
      syncError: automationEnabled && !resetSessionPending && (samplingActive || raw.state === 'unknown' && raw.read_ok === false) ? safeText(raw.error, '') : '',
      diagnostics: samplingActive ? normalizeDiagnostics(raw) : null };
  }
  function normalizeDiagnostics(raw) {
    return {
      statusSince: Date.parse(raw.status_since), badWindows: raw.quality_bad_windows, goodWindows: raw.quality_good_windows,
      sampling: raw.quality_sampling && typeof raw.quality_sampling === 'object' && !Array.isArray(raw.quality_sampling) ? raw.quality_sampling : null,
      scanLimitReached: raw.quality_scan_limit_reached === true,
      failureKind: raw.failure_kind, failureCount: raw.failure_count, temporaryFailures: raw.temporary_failures,
      recoveryAttempts: raw.recovery_attempts, recoveryFailures: raw.recovery_failures,
      lastFailureAt: Date.parse(raw.last_failure_at), lastRecoveryAttemptAt: Date.parse(raw.last_recovery_attempt_at),
      nextRetryAt: Date.parse(raw.next_retry_at), tier: raw.degradation_level, trial: raw.recovery_trial === true,
      recoveryStartedAt: Date.parse(raw.recovery_started_at), recoveryDeadline: Date.parse(raw.recovery_deadline),
      errorRequests: raw.error_metrics_available === true && Number.isInteger(raw.error_requests) && raw.error_requests >= 0 ? raw.error_requests : null,
    };
  }
  function renderStatus(result) {
    let snapshot;
    try { snapshot = JSON.parse(result.status_json || '{}'); } catch (_) { throw new Error('运行快照无法解析。'); }
    runtimeHostname = typeof snapshot.hostname === 'string' ? snapshot.hostname : runtimeHostname;
    runtimeReady = typeof snapshot.version === 'string' && snapshot.version !== '';
    const session = typeof snapshot.sampling_since === 'string' ? snapshot.sampling_since : '';
    const sessionChanged = session && session !== samplingSession;
    if (resetSessionPending && session && session !== samplingSession) resetSessionPending = false;
    if (!resetSessionPending && session) samplingSession = session;
    samplingActive = savedConfig?.automation_enabled === true && !resetSessionPending && snapshot.sampling_active === true;
    runtimePolling = !resetSessionPending && snapshot.polling === true;
    const next = Date.parse(snapshot.next_poll);
    nextPollAt = !resetSessionPending && Number.isFinite(next) ? next : null;
    const currentByID = new Map(accounts.map((account) => [account.id, sessionChanged || !samplingActive ? clearAccountSession(account) : account]));
    const runtimeAccounts = Array.isArray(snapshot.accounts) ? snapshot.accounts.map(normalizeAccount) : [];
    const runtimeByID = new Map(runtimeAccounts.map((account) => [account.id, account]));
    const configuredIDs = Array.isArray(savedConfig?.account_ids) ? savedConfig.account_ids.map(String) : [];
    accounts = configuredIDs.map((id) => runtimeByID.get(id) || currentByID.get(id) || {
      id, name: `账号 ${id}`, platform: 'unknown', group: 'temporary', status: 'ready', fixedRate: null, accountRate: null,
      upstreamRate: null, cache: null, calls: 0, letter: id.slice(-2), rateMode: 'fixed', fixedRate: 1,
    });
    setText('runtime-mode', ({ unconfigured: '未配置', monitoring: '仅监控', automatic: '自动管理', standby: '待机', error: '运行异常' })[snapshot.mode] || '未知');
    setText('runtime-storage', snapshot.storage_ready ? '可用' : '未就绪');
    const version = runtimeReady ? safeText(snapshot.version).replace(/^v/, '') : '';
    setText('runtime-version', version || '未知');
    if (version) setText('top-version', `v${version}`);
    setText('runtime-last-poll', snapshot.last_poll ? new Date(snapshot.last_poll).toLocaleString('zh-CN', { hour12: false }) : '尚未采集');
    setText('runtime-next-poll', snapshot.next_poll ? new Date(snapshot.next_poll).toLocaleString('zh-CN', { hour12: false }) : '未知');
    setText('runtime-automation-state', automationEnabled ? '已启用' : '未启用');
    if (snapshot.error) showMessage(safeText(snapshot.error), 'error');
    renderEvents(snapshot.events);
    const oldPlatform = activePlatform;
    renderGroups();
    if (oldPlatform !== activePlatform) renderRuleEditor();
    updateAddAccountControls();
  }
  async function refreshStatus() {
    if (!available || disposed || statusBusy) return;
    statusBusy = true;
    const revision = samplingRevision;
    try {
      const response = await request('plugin.status');
      if (revision === samplingRevision) renderStatus(response.result);
    }
    catch (error) { if (!disposed) $('connection-message').textContent = error.message || '状态读取失败。'; }
    finally {
      statusBusy = false;
      if (!disposed) {
        window.clearTimeout(pollTimer);
        pollTimer = window.setTimeout(refreshStatus, revision === samplingRevision ? 10000 : 0);
      }
    }
  }
  function updatePollControls() {
    const button = $('manual-poll');
    if (!button) return;
    const requesting = manualRequestBusy || runtimePolling;
    button.disabled = !available || !configLoaded || !runtimeReady || !automationEnabled || busy || requesting || resetSessionPending;
    button.textContent = requesting && automationEnabled ? '请求中…' : '手动请求';
    button.title = automationEnabled ? '立即采集本轮监控数据，不重置样本或修改配置' : '启用监控后可手动请求采集';
    let label = '未启用请求';
    if (automationEnabled) {
      if (requesting) label = '正在请求…';
      else if (nextPollAt !== null) {
        const seconds = Math.max(0, Math.ceil((nextPollAt - Date.now()) / 1000));
        label = seconds > 0 ? `下次请求 ${String(Math.floor(seconds / 60)).padStart(2, '0')}:${String(seconds % 60).padStart(2, '0')}` : '等待下一轮请求';
      } else label = '等待本轮请求';
    }
    setText('monitor-countdown', label);
  }
  async function requestManualPoll() {
    if ($('manual-poll').disabled) return;
    manualRequestBusy = true;
    const revision = samplingRevision;
    updatePollControls();
    try {
      const response = await request('config.test');
      if (!response.result?.success) throw new Error(response.result?.message || '手动请求失败。');
      if (revision === samplingRevision) {
        if (response.result.status_json) renderStatus(response.result);
        showMessage('已请求立即采集，结果将在本轮完成后更新。', 'success');
        await refreshStatus();
      }
    } catch (error) { if (!disposed) showMessage(error.message || '手动请求失败。', 'error'); }
    finally { manualRequestBusy = false; updatePollControls(); }
  }
  function scheduleInitialBridge() {
    if (!available) { $('bridge-state').textContent = '未连接宿主'; $('connection-message').textContent = '请从 Sub2API 插件配置入口打开此页。'; return; }
    runtimeReady = false;
    notify('sub2api.plugin.ready');
    request('config.load').then((response) => { applyConfig(response.config); $('bridge-state').textContent = '已连接宿主'; refreshStatus(); }).catch((error) => showMessage(error.message, 'error'));
  }

  async function clearEventLog() {
    if (!configLoaded || busy || disposed) return;
    for (const event of eventLog) hiddenEventKeys.add(eventKey(event));
    eventLog = [];
    eventPage = 1;
    renderEventPage();
    if ($('runtime-detail-dialog').open) renderAccountEvents({ id: detailAccountId });
    $('clear-events-dialog').close();
    showMessage('当前页面记录已清空。', 'success');
  }

  async function submitAddAccount() {
    const input = $('new-account-id');
    if (!configLoaded || busy) {
      $('add-account-error').textContent = !configLoaded ? '正在加载插件配置，请稍后添加。' : '正在保存配置，请稍后再试。';
      $('add-account-error').hidden = false;
      return;
    }
    const id = input.value.trim();
    const message = addAccount(id);
    $('add-account-error').textContent = message;
    $('add-account-error').hidden = !message;
    if (!message) {
      if (await saveConfig(false)) {
        input.value = '';
        scheduleAddRefresh();
      }
      else {
        accounts = accounts.filter((account) => account.id !== id);
        renderGroups();
      }
    }
  }

  function scheduleAddRefresh() {
    for (const delay of [1000, 3000, 8000]) {
      window.setTimeout(() => { if (!disposed && !statusBusy) refreshStatus(); }, delay);
    }
  }

  $('sort-direction').addEventListener('change', renderGroups);
  $('platform-select').addEventListener('change', (event) => {
    try { if (configLoaded) captureRuleEditor(); }
    catch (error) { $('platform-select').value = activePlatform; showMessage(error.message, 'error'); return; }
    activePlatform = event.target.value;
    platformSelected = true;
    renderGroups();
    renderRuleEditor();
    updateAddAccountControls();
  });
  $('add-account-button').addEventListener('click', submitAddAccount);
  $('new-account-id').addEventListener('keydown', (event) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      submitAddAccount();
    }
  });
  document.querySelectorAll('[data-close-dialog]').forEach((button) => button.addEventListener('click', () => $('account-dialog').close()));
  document.querySelectorAll('[data-close-runtime]').forEach((button) => button.addEventListener('click', () => $('runtime-detail-dialog').close()));
  document.querySelectorAll('input[name="rate_mode"]').forEach((radio) => radio.addEventListener('change', () => {
    $('edit-fixed-rate').disabled = !document.querySelector('input[name="rate_mode"][value="fixed"]').checked;
  }));
  async function saveAccountEdits(event) {
    event.preventDefault();
    if (!editingAccount || busy || !configLoaded || automationEnabled) return;
    const mode = document.querySelector('input[name="rate_mode"]:checked').value;
    const rawRate = $('edit-fixed-rate').value.trim();
    const fixedRate = Number(rawRate);
    const requiresFixed = mode === 'fixed';
    if (requiresFixed && (!rawRate || !Number.isFinite(fixedRate) || fixedRate < 0 || fixedRate > 1000000)) {
      $('edit-rate-error').textContent = '请填写 0–1000000 之间的固定倍率。';
      $('edit-rate-error').hidden = false;
      $('edit-fixed-rate').focus();
      return;
    }
    const id = editingAccount.id;
    const account = accounts.find((item) => item.id === id);
    if (!account) return;
    const previous = { group: account.group, rateMode: account.rateMode, fixedRate: account.fixedRate };
    const nextGroup = $('edit-group').value;
    if (nextGroup !== 'temporary' && mode === 'fixed' && !Number.isFinite(fixedRate)) {
      $('edit-rate-error').textContent = '进入调用组前必须填写固定倍率。';
      $('edit-rate-error').hidden = false;
      $('edit-fixed-rate').focus();
      return;
    }
    account.group = nextGroup;
    account.rateMode = mode;
    if (mode === 'fixed') account.fixedRate = fixedRate;
    $('edit-rate-error').hidden = true;
    if (await saveConfig(false)) {
      $('account-dialog').close();
      editingAccount = null;
    } else {
      const current = accounts.find((item) => item.id === id);
      if (current) Object.assign(current, previous);
      $('edit-rate-error').textContent = '保存未成功，请保留当前输入并重试。';
      $('edit-rate-error').hidden = false;
    }
    renderGroups();
  }
  $('edit-save-button').addEventListener('click', saveAccountEdits);
  $('account-edit-form').addEventListener('submit', (event) => event.preventDefault());
  $('account-edit-form').addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && event.target.id === 'edit-fixed-rate') { event.preventDefault(); saveAccountEdits(event); }
  });
  document.querySelectorAll('[data-close-delete]').forEach((button) => button.addEventListener('click', () => $('delete-account-dialog').close()));
  document.querySelectorAll('[data-close-clear]').forEach((button) => button.addEventListener('click', () => $('clear-events-dialog').close()));
  $('event-clear').addEventListener('click', () => $('clear-events-dialog').showModal());
  $('clear-events-confirm').addEventListener('click', clearEventLog);
  $('delete-account-form').addEventListener('submit', (event) => event.preventDefault());
  $('delete-account-confirm').addEventListener('click', async (event) => {
    event.preventDefault();
    if (!removingAccount || busy || !configLoaded || automationEnabled) return;
    const previous = accounts.slice();
    const wasEnabled = automationEnabled;
    const index = accounts.findIndex((account) => account.id === removingAccount.id);
    if (index !== -1) accounts.splice(index, 1);
    if (accounts.length === 0) automationEnabled = false;
    if (await saveConfig(false)) { $('delete-account-dialog').close(); removingAccount = null; }
    else { accounts = previous; automationEnabled = wasEnabled; }
    renderGroups();
  });
  $('edit-fixed-rate').addEventListener('input', () => { $('edit-rate-error').hidden = true; });
  $('automation-toggle').addEventListener('click', async () => {
    const previous = automationEnabled;
    automationEnabled = !automationEnabled;
    $('automation-enabled').checked = automationEnabled;
    renderGroups();
    if (!await saveConfig(false)) automationEnabled = previous;
    renderGroups();
  });
  $('test-connection').addEventListener('click', () => saveConfig(true));
  $('manual-poll').addEventListener('click', requestManualPoll);
  $('save-config').addEventListener('click', () => saveConfig(false));
  $('group-rules-list').addEventListener('input', updateRuleInputs);
  $('group-rules-list').addEventListener('change', updateRuleInputs);
  $('reset-rules').addEventListener('click', () => {
    if ($('reset-rules').disabled) return;
    for (const input of document.querySelectorAll('[data-rule-group] [data-field]')) {
      if (input.type === 'checkbox') input.checked = input.dataset.defaultValue === 'true';
      else input.value = input.dataset.defaultValue;
    }
    updateRuleInputs();
    $('save-message').textContent = `${platformNames[activePlatform] || activePlatform} 的分组规则与优先级已恢复默认，请保存规则后生效。`;
  });
  $('save-rules').addEventListener('click', async () => {
    if (automationEnabled) return;
    const success = await saveConfig(false, activePlatform);
    $('save-message').textContent = success ? '当前平台的调用组规则已保存。' : '未保存成功，请检查设置并重试。';
  });
  $('event-prev').addEventListener('click', () => { eventPage -= 1; renderEventPage(); });
  $('event-next').addEventListener('click', () => { eventPage += 1; renderEventPage(); });

  window.addEventListener('message', receiveMessage);
  window.addEventListener('pagehide', () => {
    disposed = true;
    window.clearTimeout(pollTimer);
    window.clearInterval(countdownTimer);
    for (const entry of pending.values()) { window.clearTimeout(entry.timer); entry.reject(new Error('页面已关闭。')); }
    pending.clear();
  });
  renderGroups();
  renderRuleEditor();
  updateAddAccountControls();
  countdownTimer = window.setInterval(() => {
    updatePollControls();
    document.querySelectorAll('.monitor-retry-countdown').forEach(updateMonitorRetry);
    if ($('runtime-detail-dialog').open) updateDetailRetry(accounts.find((account) => account.id === detailAccountId));
  }, 1000);
  scheduleInitialBridge();
})();
