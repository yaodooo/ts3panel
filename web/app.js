(() => {
  'use strict';

  const i18n = new Map();
  const originalLabels = new WeakMap();
  let toastTimer = null;

  const usernamePattern = /^[A-Za-z0-9_-]{3,32}$/;
  const domainPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$/i;

  class ApiError extends Error {
    constructor(code, message, hint, status, retryAfter, payload) {
      super(message || code || 'request failed');
      this.name = 'ApiError';
      this.code = code || 'E_UNKNOWN';
      this.hint = hint || '';
      this.status = status || 0;
      this.retryAfter = retryAfter || 0;
      // payload keeps the full error body: DNS failures carry the manual record
      // checklist and the retryable flag, which the failure modal needs.
      this.payload = payload || null;
    }
  }

  function qs(selector, root) {
    return (root || document).querySelector(selector);
  }

  function qsa(selector, root) {
    return Array.from((root || document).querySelectorAll(selector));
  }

  function t(key, fallback) {
    const value = i18n.get(key);
    if (value !== undefined && value !== null && value !== '') {
      return value;
    }
    if (fallback !== undefined && fallback !== null) {
      return fallback;
    }
    return key;
  }

  function format(template, vars) {
    return String(template).replace(/\{(\w+)\}/g, (match, name) => {
      return Object.prototype.hasOwnProperty.call(vars, name) ? String(vars[name]) : match;
    });
  }

  function tf(key, vars, fallback) {
    return format(t(key, fallback), vars);
  }

  function applyI18n() {
    for (const el of qsa('[data-i18n]')) {
      const value = i18n.get(el.dataset.i18n);
      if (value) {
        el.textContent = value;
      }
    }
    for (const el of qsa('[data-i18n-placeholder]')) {
      const value = i18n.get(el.dataset.i18nPlaceholder);
      if (value) {
        el.setAttribute('placeholder', value);
      }
    }
    for (const el of qsa('[data-i18n-aria-label]')) {
      const value = i18n.get(el.dataset.i18nAriaLabel);
      if (value) {
        el.setAttribute('aria-label', value);
      }
    }
    for (const el of qsa('[data-i18n-title]')) {
      const value = i18n.get(el.dataset.i18nTitle);
      if (value) {
        el.setAttribute('title', value);
      }
    }
  }

  async function loadI18n() {
    try {
      const res = await fetch('/i18n/zh-CN.json', { cache: 'no-store' });
      if (!res.ok) {
        return;
      }
      const data = await res.json();
      for (const [key, value] of Object.entries(data)) {
        if (typeof value === 'string') {
          i18n.set(key, value);
        }
      }
    } catch (err) {
      // The HTML contains Chinese fallbacks; a failed dictionary fetch must not
      // block the panel.
      console.warn('i18n load failed', err);
    }
  }

  function showToast(message, kind) {
    const toast = qs('#toast');
    if (!toast) {
      return;
    }
    toast.textContent = message;
    toast.className = 'toast' + (kind ? ' is-' + kind : '');
    toast.hidden = false;
    if (toastTimer) {
      window.clearTimeout(toastTimer);
    }
    toastTimer = window.setTimeout(() => {
      toast.hidden = true;
    }, 4200);
  }

  function setButtonBusy(button, busy, busyText) {
    if (!button) {
      return;
    }
    if (busy) {
      if (!originalLabels.has(button)) {
        originalLabels.set(button, button.textContent);
      }
      button.disabled = true;
      if (busyText) {
        button.textContent = busyText;
      }
      return;
    }
    const original = originalLabels.get(button);
    if (original !== undefined) {
      button.textContent = original;
    }
    originalLabels.delete(button);
    button.disabled = false;
  }

  async function api(path, options) {
    const opts = options || {};
    const headers = {
      Accept: 'application/json',
      'X-Requested-With': 'ts3panel',
    };
    const fetchOptions = {
      method: opts.method || 'GET',
      headers,
      credentials: 'same-origin',
      cache: 'no-store',
    };
    if (opts.form !== undefined) {
      // FormData must set its own multipart boundary; never add Content-Type.
      fetchOptions.body = opts.form;
    } else if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      fetchOptions.body = JSON.stringify(opts.body);
    }

    let res;
    try {
      res = await fetch(path, fetchOptions);
    } catch (err) {
      throw new ApiError('E_NETWORK', t('errors.network'), '', 0, 0);
    }

    const contentType = (res.headers.get('content-type') || '').toLowerCase();
    if (!contentType.includes('application/json')) {
      if (res.redirected && new URL(res.url, window.location.href).pathname === '/setup') {
        window.location.assign('/setup');
        throw new ApiError('E_REDIRECT', '', '', res.status, 0);
      }
      if (res.status === 204) {
        return {};
      }
      throw new ApiError('E_BAD_RESPONSE', t('errors.bad_response'), '', res.status, 0);
    }

    let data;
    try {
      data = await res.json();
    } catch (err) {
      throw new ApiError('E_BAD_RESPONSE', t('errors.bad_response'), '', res.status, 0);
    }

    if (!res.ok) {
      const body = data && data.error ? data.error : {};
      const retryAfter = Number.parseInt(res.headers.get('retry-after') || '', 10);
      throw new ApiError(
        body.code || 'E_UNKNOWN',
        body.message || t('errors.unknown'),
        body.hint || '',
        res.status,
        Number.isFinite(retryAfter) ? retryAfter : 0,
        data
      );
    }
    return data;
  }

  function formatApiError(err) {
    if (!(err instanceof ApiError)) {
      return t('errors.network');
    }
    if (err.code === 'E_RATE_LIMIT') {
      const seconds = err.retryAfter > 0 ? err.retryAfter : 900;
      return tf('errors.rate_limited', { seconds }, '尝试次数过多，请 ' + seconds + ' 秒后再试。');
    }
    let message = err.message || t('errors.unknown');
    if (err.hint) {
      message += ' ' + err.hint;
    }
    return message;
  }

  function passwordProblem(password) {
    if ([...password].length < 10) {
      return t('setup.error_password');
    }
    let bytes = password.length;
    try {
      bytes = new TextEncoder().encode(password).length;
    } catch (err) {
      bytes = password.length;
    }
    if (bytes > 72) {
      return t('setup.error_password');
    }
    if (!/\p{L}/u.test(password) || !/\p{Nd}/u.test(password)) {
      return t('setup.error_password');
    }
    return '';
  }

  function isIPv4(value) {
    const parts = value.split('.');
    if (parts.length !== 4) {
      return false;
    }
    return parts.every((part) => /^\d{1,3}$/.test(part) && Number(part) <= 255);
  }

  function formatDate(value) {
    if (!value) {
      return '';
    }
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) {
      return '';
    }
    try {
      return date.toLocaleString('zh-CN', {
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
      });
    } catch (err) {
      return date.toISOString();
    }
  }

  async function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return;
    }
    const textarea = document.createElement('textarea');
    textarea.value = text;
    textarea.className = 'clipboard-helper';
    textarea.setAttribute('readonly', '');
    document.body.appendChild(textarea);
    textarea.select();
    const ok = document.execCommand('copy');
    textarea.remove();
    if (!ok) {
      throw new Error('copy failed');
    }
  }

  function downloadText(filename, text) {
    const blob = new Blob([text + '\n'], { type: 'text/plain;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = filename;
    anchor.rel = 'noopener';
    anchor.hidden = true;
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 2000);
  }

  async function initSetup() {
    const loading = qs('#setup-loading');
    const blocked = qs('#setup-blocked');
    const blockedTitle = qs('#setup-blocked h1');
    const blockedMessage = qs('#setup-blocked-message');
    const goLogin = qs('#setup-go-login');
    const retry = qs('#setup-retry');
    const setupCard = qs('#setup-app');
    const setupTitle = qs('#setup-title');
    const setupSubtitle = qs('#setup-subtitle');
    const modeBadge = qs('#setup-mode-badge');
    const stepper = qs('#setup-stepper');
    const stepItems = qsa('#setup-stepper .step');
    const panels = qsa('[data-step-panel]');
    const errorBox = qs('#setup-error');
    const usernameInput = qs('#setup-username');
    const passwordInput = qs('#setup-password');
    const passwordConfirm = qs('#setup-password-confirm');
    const accountTitle = qs('[data-step-panel="1"] h2');
    const step1Next = qs('#step1-next');
    const step2Back = qs('#step2-back');
    const step2Next = qs('#step2-next');
    const providerFields = qs('#provider-fields');
    const providerLabel = qs('#provider-label');
    const providerZone = qs('#provider-zone');
    const providerRadios = qsa('input[name="provider"]');
    const publicIP = qs('#setup-public-ip');
    const step3Back = qs('#step3-back');
    const step3Submit = qs('#step3-submit');
    const recoveryGrid = qs('#recovery-grid');
    const codesWarning = qs('[data-step-panel="4"] .alert-warning');
    const copyCodes = qs('#copy-codes');
    const downloadCodes = qs('#download-codes');
    const savedCodes = qs('#saved-codes');
    const finishSetup = qs('#finish-setup');

    let mode = 'initial';
    let resetToken = new URLSearchParams(window.location.search).get('token') || '';
    let currentStep = 1;
    const values = { provider: null, codes: [] };

    function showSetupError(message) {
      errorBox.textContent = message;
      errorBox.className = 'alert alert-error';
      errorBox.hidden = false;
    }

    function clearSetupError() {
      errorBox.hidden = true;
      errorBox.textContent = '';
    }

    function showBlocked(title, message, kind) {
      loading.hidden = true;
      setupCard.hidden = true;
      blocked.hidden = false;
      blockedTitle.textContent = title;
      blockedMessage.textContent = message;
      const retryMode = kind === 'retry';
      goLogin.hidden = retryMode;
      retry.hidden = !retryMode;
    }

    function setStep(step) {
      currentStep = step;
      for (const panel of panels) {
        panel.hidden = Number(panel.dataset.stepPanel) !== step;
      }
      if (mode === 'reset') {
        stepper.hidden = true;
      } else {
        stepper.hidden = false;
        for (const item of stepItems) {
          const itemStep = Number(item.dataset.step);
          item.classList.toggle('is-active', itemStep === step);
          item.classList.toggle('is-done', itemStep < step);
          if (itemStep === step) {
            item.setAttribute('aria-current', 'step');
          } else {
            item.removeAttribute('aria-current');
          }
        }
      }
      clearSetupError();
      if (step === 1) {
        const first = qs('input', qs('[data-step-panel="1"]'));
        if (first) {
          first.focus();
        }
      }
    }

    function syncProviderFields() {
      const selected = qs('input[name="provider"]:checked');
      const value = selected ? selected.value : 'skip';
      for (const card of qsa('.provider-card')) {
        card.classList.toggle('is-selected', selected !== null && card.contains(selected));
      }
      providerFields.hidden = value === 'skip';
      for (const block of qsa('[data-provider-fields]', providerFields)) {
        block.hidden = block.dataset.providerFields !== value;
      }
    }

    function collectProvider() {
      const selected = qs('input[name="provider"]:checked');
      if (!selected || selected.value === 'skip') {
        return null;
      }
      const type = selected.value;
      const zone = providerZone.value.trim().toLowerCase();
      if (!domainPattern.test(zone)) {
        showSetupError(t('setup.provider_error_zone'));
        providerZone.focus();
        return undefined;
      }
      const creds = {};
      if (type === 'cloudflare') {
        const token = qs('#cf-token').value.trim();
        if (!token) {
          showSetupError(t('setup.provider_error_creds'));
          qs('#cf-token').focus();
          return undefined;
        }
        creds.api_token = token;
      } else if (type === 'aliyun') {
        const keyID = qs('#ali-key-id').value.trim();
        const keySecret = qs('#ali-key-secret').value.trim();
        if (!keyID || !keySecret) {
          showSetupError(t('setup.provider_error_creds'));
          qs('#ali-key-id').focus();
          return undefined;
        }
        creds.access_key_id = keyID;
        creds.access_key_secret = keySecret;
      } else if (type === 'dnspod') {
        const secretID = qs('#dnspod-secret-id').value.trim();
        const secretKey = qs('#dnspod-secret-key').value.trim();
        if (!secretID || !secretKey) {
          showSetupError(t('setup.provider_error_creds'));
          qs('#dnspod-secret-id').focus();
          return undefined;
        }
        creds.secret_id = secretID;
        creds.secret_key = secretKey;
      }
      return {
        type,
        label: providerLabel.value.trim(),
        zone,
        creds,
      };
    }

    function renderRecoveryCodes(codes, notice) {
      recoveryGrid.replaceChildren();
      for (const code of codes) {
        const span = document.createElement('span');
        span.className = 'recovery-code';
        span.textContent = String(code);
        recoveryGrid.appendChild(span);
      }
      if (notice) {
        codesWarning.textContent = String(notice);
      }
      savedCodes.checked = false;
      finishSetup.disabled = true;
    }

    async function submitAccount(isReset) {
      const username = usernameInput.value.trim();
      const body = {
        username,
        password: passwordInput.value,
      };
      if (isReset) {
        body.setup_token = resetToken;
      } else {
        body.public_ip = publicIP.value.trim();
        if (values.provider) {
          body.provider = values.provider;
        }
        // Non-loopback first-run setup requires the one-time token from
        // `ts3panel reset-password`; the setup page receives it via ?token=.
        if (resetToken) {
          body.setup_token = resetToken;
        }
      }

      const primary = isReset ? step1Next : step3Submit;
      const back = isReset ? null : step3Back;
      setButtonBusy(primary, true, isReset ? t('setup.resetting') : t('setup.submitting'));
      if (back) {
        back.disabled = true;
      }

      try {
        const result = await api('/api/setup', { method: 'POST', body });
        if (!result || !Array.isArray(result.recovery_codes) || result.recovery_codes.length === 0) {
          throw new ApiError('E_BAD_RESPONSE', t('errors.bad_response'), '', 0, 0);
        }
        values.codes = result.recovery_codes;
        if (resetToken) {
          resetToken = '';
          window.history.replaceState(null, '', '/setup');
        }
        passwordInput.value = '';
        passwordConfirm.value = '';
        renderRecoveryCodes(result.recovery_codes, result.notice);
        setStep(4);
      } catch (err) {
        if (err instanceof ApiError && err.code === 'E_SETUP_DONE') {
          showSetupError(t('setup.error_login_again'));
          window.setTimeout(() => window.location.assign('/'), 1800);
        } else if (err instanceof ApiError && err.code === 'E_SETUP_TOKEN') {
          showSetupError(t('setup.invalid_token'));
        } else {
          showSetupError(formatApiError(err));
        }
      } finally {
        setButtonBusy(primary, false);
        if (back) {
          back.disabled = false;
        }
      }
    }

    async function handleStep1() {
      const username = usernameInput.value.trim();
      if (!usernamePattern.test(username)) {
        showSetupError(t('setup.error_username'));
        usernameInput.focus();
        return;
      }
      const problem = passwordProblem(passwordInput.value);
      if (problem) {
        showSetupError(problem);
        passwordInput.focus();
        return;
      }
      if (passwordInput.value !== passwordConfirm.value) {
        showSetupError(t('setup.error_password_mismatch'));
        passwordConfirm.focus();
        return;
      }
      clearSetupError();
      if (mode === 'reset') {
        await submitAccount(true);
        return;
      }
      setStep(2);
    }

    async function handleStep2() {
      const provider = collectProvider();
      if (provider === undefined) {
        return;
      }
      if (!provider) {
        setButtonBusy(step2Next, true, t('common.loading'));
        try {
          await api('/api/setup/skip-dns', { method: 'POST' });
        } catch (err) {
          showSetupError(formatApiError(err));
          return;
        } finally {
          setButtonBusy(step2Next, false);
        }
        values.provider = null;
        setStep(3);
        return;
      }
      values.provider = provider;
      setStep(3);
    }

    async function handleStep3() {
      const ip = publicIP.value.trim();
      if (ip && !isIPv4(ip)) {
        showSetupError(t('setup.ip_error'));
        publicIP.focus();
        return;
      }
      await submitAccount(false);
    }

    step1Next.addEventListener('click', () => {
      void handleStep1();
    });
    step2Back.addEventListener('click', () => {
      setStep(1);
    });
    step2Next.addEventListener('click', () => {
      void handleStep2();
    });
    step3Back.addEventListener('click', () => {
      setStep(2);
    });
    step3Submit.addEventListener('click', () => {
      void handleStep3();
    });
    for (const radio of providerRadios) {
      radio.addEventListener('change', syncProviderFields);
    }
    savedCodes.addEventListener('change', () => {
      finishSetup.disabled = !savedCodes.checked;
    });
    finishSetup.addEventListener('click', () => {
      if (!savedCodes.checked) {
        return;
      }
      window.location.assign('/');
    });
    copyCodes.addEventListener('click', async () => {
      if (!values.codes.length) {
        return;
      }
      try {
        await copyText(values.codes.join('  '));
        showToast(t('setup.copy_ok'), 'success');
      } catch (err) {
        showToast(t('setup.copy_failed'), 'error');
      }
    });
    downloadCodes.addEventListener('click', () => {
      if (!values.codes.length) {
        return;
      }
      const lines = [
        t('setup.download_title'),
        new Date().toLocaleString('zh-CN'),
        '',
        ...values.codes,
        '',
        t('setup.codes_warning'),
      ];
      downloadText('ts3panel-recovery-codes.txt', lines.join('\n'));
    });
    setupCard.addEventListener('keydown', (event) => {
      if (event.key !== 'Enter' || event.defaultPrevented) {
        return;
      }
      const target = event.target;
      if (!(target instanceof HTMLElement) || target.tagName !== 'INPUT') {
        return;
      }
      if (target.type === 'checkbox' || target.type === 'radio') {
        return;
      }
      event.preventDefault();
      if (currentStep === 1) {
        step1Next.click();
      } else if (currentStep === 2) {
        step2Next.click();
      } else if (currentStep === 3) {
        step3Submit.click();
      }
    });
    retry.addEventListener('click', () => {
      window.location.reload();
    });

    let setupState;
    try {
      const query = resetToken ? '?token=' + encodeURIComponent(resetToken) : '';
      setupState = await api('/api/setup/state' + query);
    } catch (err) {
      showBlocked(t('setup.load_error_title'), t('setup.load_error'), 'retry');
      return;
    }

    loading.hidden = true;

    if (setupState.setup_completed) {
      if (!setupState.reset_allowed) {
        if (resetToken) {
          showBlocked(t('setup.invalid_token_title'), t('setup.invalid_token'), 'login');
        } else {
          showBlocked(t('setup.completed_title'), t('setup.completed_desc'), 'login');
        }
        return;
      }

      mode = 'reset';
      modeBadge.hidden = false;
      setupTitle.textContent = t('setup.reset_title');
      setupSubtitle.textContent = t('setup.reset_subtitle');
      accountTitle.textContent = t('setup.reset_title');
      step1Next.textContent = t('setup.reset_submit');
      if (setupState.username) {
        usernameInput.value = String(setupState.username);
      }
    } else {
      if (setupState.public_ip) {
        publicIP.value = String(setupState.public_ip);
      }
      if (setupState.setup_dns_skipped) {
        const skip = providerRadios.find((radio) => radio.value === 'skip');
        if (skip) {
          skip.checked = true;
        }
      }
    }

    syncProviderFields();
    setupCard.hidden = false;
    setStep(1);
  }

  async function initIndex() {
      const ui = {
        loading: qs('#app-loading'),
        authView: qs('#auth-view'),
        dashboardView: qs('#dashboard-view'),
        authError: qs('#auth-error'),
        loginPanel: qs('#login-panel'),
        forgotPanel: qs('#forgot-panel'),
        loginForm: qs('#login-form'),
        loginUsername: qs('#login-username'),
        loginPassword: qs('#login-password'),
        loginSubmit: qs('#login-submit'),
        showForgot: qs('#show-forgot'),
        forgotForm: qs('#forgot-form'),
        forgotBack: qs('#forgot-back'),
        forgotSubmit: qs('#forgot-submit'),
        forgotUsername: qs('#forgot-username'),
        forgotCode: qs('#forgot-code'),
        forgotPassword: qs('#forgot-password'),
        forgotConfirm: qs('#forgot-confirm'),
        logout: qs('#logout'),
        topbarUser: qs('#topbar-user'),
        currentUser: qs('#current-user'),
        navInstances: qs('#nav-instances'),
        navPorts: qs('#nav-ports'),
        navSettings: qs('#nav-settings'),
        viewInstances: qs('#view-instances'),
        viewDetail: qs('#view-detail'),
        viewPorts: qs('#view-ports'),
        viewSettings: qs('#view-settings'),
        statInstances: qs('#stat-instances'),
        statRunning: qs('#stat-running'),
        statDNS: qs('#stat-dns'),
        newInstance: qs('#new-instance'),
        portsLoading: qs('#ports-loading'),
        portsError: qs('#ports-error'),
        portsContent: qs('#ports-content'),
        portsStats: qs('#ports-stats'),
        portsPublic: qs('#ports-public'),
        portsInternalRanges: qs('#ports-internal-ranges'),
        portsText: qs('#ports-text'),
        copyPortChecklist: qs('#copy-port-checklist'),
        refreshPorts: qs('#refresh-ports'),
        checkPorts: qs('#check-ports'),
        portsCheckNote: qs('#ports-check-note'),
        portsCheckLoading: qs('#ports-check-loading'),
        portsCheckResult: qs('#ports-check-result'),
        instancesLoading: qs('#instances-loading'),
        instancesError: qs('#instances-error'),
        instancesEmpty: qs('#instances-empty'),
        instancesList: qs('#instances-list'),
        detailBack: qs('#detail-back'),
        detailSkeleton: qs('#detail-skeleton'),
        detailError: qs('#detail-error'),
        detailErrorMessage: qs('#detail-error-message'),
        detailRetry: qs('#detail-retry'),
        detailErrorBack: qs('#detail-error-back'),
        detailContent: qs('#detail-content'),
        detailName: qs('#detail-name'),
        detailSubdomain: qs('#detail-subdomain'),
        detailBadges: qs('#detail-badges'),
        detailConnection: qs('#detail-connection'),
        tokenHint: qs('#token-hint'),
        detailToken: qs('#detail-token'),
        tokenToggle: qs('#token-toggle'),
        tokenCopy: qs('#token-copy'),
        detailPorts: qs('#detail-ports'),
        copyPorts: qs('#copy-ports'),
        detailStats: qs('#detail-stats'),
        detailStatsNote: qs('#detail-stats-note'),
        refreshDetail: qs('#refresh-detail'),
        detailDNSBadge: qs('#detail-dns-badge'),
        detailDNSManualNote: qs('#detail-dns-manual-note'),
        detailDNSLine: qs('#detail-dns-line'),
        detailDNSDetail: qs('#detail-dns-detail'),
        detailVerifyResult: qs('#detail-verify-result'),
        bindDomain: qs('#bind-domain'),
        verifyDomain: qs('#verify-domain'),
        manualDomain: qs('#manual-domain'),
        unbindDomain: qs('#unbind-domain'),
        detailLogMeta: qs('#detail-log-meta'),
        detailLogsFallback: qs('#detail-logs-fallback'),
        detailLogs: qs('#detail-logs'),
        detailStart: qs('#detail-start'),
        detailStop: qs('#detail-stop'),
        detailRestart: qs('#detail-restart'),
        deleteInstance: qs('#delete-instance'),
        providersLoading: qs('#providers-loading'),
        providersError: qs('#providers-error'),
        providersEmpty: qs('#providers-empty'),
        providersList: qs('#providers-list'),
        addProvider: qs('#add-provider'),
        settingsPublicIP: qs('#settings-public-ip'),
        settingsDetectLine: qs('#settings-detect-line'),
        useDetectedIP: qs('#use-detected-ip'),
        savePublicIP: qs('#save-public-ip'),
        settingsIPAlert: qs('#settings-ip-alert'),
        backupMeta: qs('#backup-meta'),
        backupAlert: qs('#backup-alert'),
        createBackup: qs('#create-backup'),
        uploadBackup: qs('#upload-backup'),
        refreshBackups: qs('#refresh-backups'),
        backupsLoading: qs('#backups-loading'),
        backupsError: qs('#backups-error'),
        backupsEmpty: qs('#backups-empty'),
        backupsList: qs('#backups-list'),
        backupEnabled: qs('#backup-enabled'),
        backupMode: qs('#backup-mode'),
        backupDailyAt: qs('#backup-daily-at'),
        backupIntervalHours: qs('#backup-interval-hours'),
        backupRetention: qs('#backup-retention'),
        backupPlanState: qs('#backup-plan-state'),
        backupPlanStatus: qs('#backup-plan-status'),
        backupPlanAlert: qs('#backup-plan-alert'),
        saveBackupPlan: qs('#save-backup-plan'),
        refreshBackupPlan: qs('#refresh-backup-plan'),
        settingsExternalCheck: qs('#settings-external-check'),
        saveExternalCheck: qs('#save-external-check'),
        externalCheckAlert: qs('#external-check-alert'),
        refreshAllBackups: qs('#refresh-all-backups'),
        allBackupsLoading: qs('#all-backups-loading'),
        allBackupsAlert: qs('#all-backups-alert'),
        allBackupsList: qs('#all-backups-list'),
        modalRoot: qs('#modal-root'),
        modalTitle: qs('#modal-title'),
        modalBody: qs('#modal-body'),
        modalClose: qs('#modal-close'),
      };

      let authenticated = false;
      let routeBound = false;
      let listTimer = null;
      let detailTimer = null;
      let currentView = 'instances';
      let currentDetail = null;
      let currentDetailID = '';
      let detailRequestID = 0;
      let tokenVisible = false;
      let verifyNotice = null;
      let portsData = null;
      let providers = [];
      let providersRequest = null;
      let detectedIP = '';
      let publicIP = '';
      let backupPlan = null;
      let lastFocus = null;
      let closeHandler = null;
      let fieldSeq = 0;

      // ------------------------------------------------------------------ utils

      function el(tag, className, text) {
        const node = document.createElement(tag);
        if (className) {
          node.className = className;
        }
        if (text !== undefined && text !== null) {
          node.textContent = String(text);
        }
        return node;
      }

      function field(labelText, input, hintText, extraClass) {
        fieldSeq += 1;
        if (!input.id) {
          input.id = 'field-' + fieldSeq;
        }
        const wrap = el('div', 'field' + (extraClass ? ' ' + extraClass : ''));
        const label = el('label', '', labelText);
        label.htmlFor = input.id;
        wrap.appendChild(label);
        wrap.appendChild(input);
        if (hintText) {
          wrap.appendChild(el('p', 'hint', hintText));
        }
        return wrap;
      }

      function textInput(value, placeholder, maxLength) {
        const input = el('input');
        input.type = 'text';
        input.autocomplete = 'off';
        input.spellcheck = false;
        if (value) {
          input.value = String(value);
        }
        if (placeholder) {
          input.placeholder = placeholder;
        }
        if (maxLength) {
          input.maxLength = maxLength;
        }
        return input;
      }

      function button(label, className) {
        const node = el('button', 'button ' + (className || ''));
        node.type = 'button';
        node.textContent = label;
        return node;
      }

      function setText(node, value) {
        if (node) {
          node.textContent = value === undefined || value === null ? '' : String(value);
        }
      }

      function showToastSafe(message, kind) {
        showToast(message, kind);
      }

      function statusClass(status) {
        return {
          pending: 'is-pending',
          running: 'is-running',
          stopped: 'is-stopped',
          exited: 'is-stopped',
          missing: 'is-stopped',
          failed: 'is-failed',
          ready: 'is-ready',
        }[status] || '';
      }

      function dnsClass(status) {
        return {
          none: 'is-dns-none',
          pending: 'is-dns-pending',
          verified: 'is-dns-verified',
          failed: 'is-dns-failed',
        }[status] || '';
      }

      function statusLabel(status) {
        const known = ['pending', 'running', 'stopped', 'exited', 'missing', 'failed', 'ready'];
        return known.includes(status) ? t('status.' + status) : t('status.unknown');
      }

      function dnsLabel(status) {
        const known = ['none', 'pending', 'verified', 'failed'];
        return known.includes(status) ? t('dns.' + status) : t('dns.unknown');
      }

      function portValue(value) {
        const n = Number(value);
        return Number.isInteger(n) && n > 0 ? n : '—';
      }

      function formatBytes(value) {
        let n = Number(value);
        if (!Number.isFinite(n) || n <= 0) {
          return '0 B';
        }
        const units = ['B', 'KB', 'MB', 'GB', 'TB'];
        let index = 0;
        while (n >= 1024 && index < units.length - 1) {
          n /= 1024;
          index += 1;
        }
        const digits = index === 0 || n >= 100 ? 0 : 1;
        return n.toFixed(digits) + ' ' + units[index];
      }

      function formatCPU(stats) {
        if (!stats || !stats.available || !stats.cpu_available) {
          return '';
        }
        const value = Number(stats.cpu_percent);
        if (!Number.isFinite(value)) {
          return '';
        }
        return (Math.round(value * 100) / 100).toString() + '%';
      }

      function isFullDomain(value) {
        return typeof value === 'string' && value.includes('.');
      }

      function handleUnauthorized(err) {
        if (err instanceof ApiError && err.status === 401) {
          showAuth();
          showAuthPanel('login', false);
          showAuthAlert(t('login.session_expired'));
          return true;
        }
        return false;
      }

      function providerTypeLabel(type) {
        return {
          cloudflare: 'Cloudflare',
          aliyun: t('setup.provider_aliyun', '阿里云 DNS'),
          dnspod: 'DNSPod',
          manual: t('setup.provider_manual', '手动解析'),
        }[type] || String(type || '');
      }

      function findProvider(id) {
        return providers.find((item) => item.id === id) || null;
      }

      // --------------------------------------------------------------- navigation

      function setActiveView(name) {
        currentView = name;
        ui.viewInstances.hidden = name !== 'instances';
        ui.viewDetail.hidden = name !== 'detail';
        ui.viewPorts.hidden = name !== 'ports';
        ui.viewSettings.hidden = name !== 'settings';
        ui.navInstances.classList.toggle('is-active', name === 'instances' || name === 'detail');
        ui.navPorts.classList.toggle('is-active', name === 'ports');
        ui.navSettings.classList.toggle('is-active', name === 'settings');
      }

      function navigate(hash) {
        if (window.location.hash === hash) {
          void route();
          return;
        }
        window.location.hash = hash;
      }

      async function route() {
        if (!authenticated) {
          return;
        }
        const hash = window.location.hash || '#/';
        const detailMatch = /^#\/instances\/([^/]+)$/.exec(hash);
        if (detailMatch) {
          await showDetail(decodeURIComponent(detailMatch[1]));
          return;
        }
        if (hash === '#/settings') {
          await showSettings();
          return;
        }
        if (hash === '#/ports') {
          await showPorts();
          return;
        }
        await showInstances();
      }

      function stopListPolling() {
        if (listTimer !== null) {
          window.clearInterval(listTimer);
          listTimer = null;
        }
      }

      function startListPolling() {
        stopListPolling();
        listTimer = window.setInterval(() => {
          if (currentView === 'instances' && !document.hidden) {
            void loadInstances(false);
          }
        }, 10000);
      }

      function stopDetailPolling() {
        if (detailTimer !== null) {
          window.clearInterval(detailTimer);
          detailTimer = null;
        }
      }

      function startDetailPolling() {
        stopDetailPolling();
        detailTimer = window.setInterval(() => {
          if (currentView === 'detail' && !document.hidden && currentDetailID) {
            void loadDetail(currentDetailID, false);
          }
        }, 10000);
      }

      async function showInstances() {
        stopDetailPolling();
        setActiveView('instances');
        startListPolling();
        await loadInstances(true);
      }

      async function showSettings() {
        stopListPolling();
        stopDetailPolling();
        setActiveView('settings');
        await Promise.all([loadProviders(true), loadPublicIPSettings(), loadBackupPlan(), loadAllBackups()]);
      }

      async function showDetail(id) {
        stopListPolling();
        setActiveView('detail');
        currentDetailID = id;
        await loadDetail(id, true);
        startDetailPolling();
      }

      async function showPorts() {
        stopListPolling();
        stopDetailPolling();
        setActiveView('ports');
        await loadPorts(true);
      }

      // ------------------------------------------------------------------- ports

      function formatPortRange(range) {
        const start = portValue(range && range.start);
        const end = portValue(range && range.end);
        if (start === '—') {
          return '—';
        }
        return start === end ? String(start) : start + '-' + end;
      }

      function portProtocolBadge(protocol) {
        const value = String(protocol || '').toUpperCase();
        const kind = value === 'UDP' ? 'is-udp' : 'is-tcp';
        return el('span', 'port-protocol ' + kind, value || '—');
      }

      function buildPortRangeTable(ranges, emptyText) {
        if (!Array.isArray(ranges) || ranges.length === 0) {
          return el('p', 'muted ports-empty', emptyText);
        }
        const table = el('table', 'port-table ports-table');
        const head = el('thead');
        const headRow = el('tr');
        for (const label of [
          t('ports.protocol', '协议'),
          t('ports.range', '端口范围'),
          t('ports.purpose', '用途'),
        ]) {
          headRow.appendChild(el('th', '', label));
        }
        head.appendChild(headRow);
        table.appendChild(head);
        const body = el('tbody');
        for (const item of ranges) {
          const tr = el('tr');
          const protocolCell = el('td');
          protocolCell.appendChild(portProtocolBadge(item.protocol));
          tr.appendChild(protocolCell);
          tr.appendChild(el('td', 'port-number', formatPortRange(item)));
          tr.appendChild(el('td', '', item.purpose || '—'));
          body.appendChild(tr);
        }
        table.appendChild(body);
        return table;
      }

      function buildPortsStat(label, value) {
        const card = el('article', 'card stat-card');
        card.appendChild(el('span', 'stat-label', label));
        card.appendChild(el('strong', 'stat-value', String(value)));
        return card;
      }

      function renderPortsPage(data) {
        portsData = data;
        ui.portsStats.replaceChildren(
          buildPortsStat(t('ports.stat_instances', '实例'), data.instance_count || 0),
          buildPortsStat(t('ports.stat_public', '待开放规则'), Array.isArray(data.public) ? data.public.length : 0),
          buildPortsStat(t('ports.stat_internal', '仅本机端口'), Array.isArray(data.do_not_open) ? data.do_not_open.length : 0)
        );
        ui.portsPublic.replaceChildren(buildPortRangeTable(
          data.public,
          t('ports.empty_public', '当前没有实例，无需开放端口。')
        ));
        ui.portsInternalRanges.replaceChildren(buildPortRangeTable(
          data.do_not_open,
          t('ports.empty_internal', '当前没有需要保留在本机的查询端口。')
        ));
        setText(ui.portsText, data.text || '');
        ui.copyPortChecklist.disabled = !data.text;
        ui.portsContent.hidden = false;
      }

      async function loadPorts(initial) {
        if (initial) {
          ui.portsLoading.hidden = false;
          ui.portsContent.hidden = true;
          ui.portsCheckLoading.hidden = true;
          ui.portsCheckNote.hidden = true;
          ui.portsCheckResult.hidden = true;
        }
        ui.portsError.hidden = true;
        try {
          const data = await api('/api/ports');
          if (!data || !Array.isArray(data.public) || !Array.isArray(data.do_not_open)) {
            throw new ApiError('E_BAD_RESPONSE', t('errors.bad_response'), '', 0, 0);
          }
          renderPortsPage(data);
        } catch (err) {
          if (handleUnauthorized(err)) {
            return;
          }
          setText(ui.portsError, formatApiError(err));
          ui.portsError.hidden = false;
          ui.portsContent.hidden = true;
        } finally {
          ui.portsLoading.hidden = true;
        }
      }

      function checkStateClass(state) {
        return {
          open: 'is-open',
          closed: 'is-closed',
          unknown: 'is-unknown',
        }[state] || 'is-unknown';
      }

      function checkStateLabel(state) {
        return {
          open: t('ports.state_open', '可达'),
          closed: t('ports.state_closed', '不可达'),
          unknown: t('ports.state_unknown', '未验证'),
        }[state] || t('ports.state_unknown', '未验证');
      }

      function renderPortsCheck(data) {
        ui.portsCheckNote.hidden = false;
        ui.portsCheckNote.className = 'alert alert-warning';
        setText(ui.portsCheckNote, data.note || t('ports.check_note_default',
          '检测结果为辅助参考，最终以客户端能否连接为准。'));

        ui.portsCheckResult.replaceChildren();
        const results = Array.isArray(data.results) ? data.results : [];
        if (results.length === 0) {
          ui.portsCheckResult.appendChild(el('p', 'muted',
            t('ports.check_empty', '没有需要检测的端口。')));
        } else {
          const table = el('table', 'port-table ports-check-table');
          const head = el('thead');
          const headRow = el('tr');
          for (const label of [
            t('ports.check_instance', '实例'),
            t('ports.check_port', '端口'),
            t('ports.protocol', '协议'),
            t('ports.check_result', '结果'),
            t('ports.check_detail', '说明'),
          ]) {
            headRow.appendChild(el('th', '', label));
          }
          head.appendChild(headRow);
          table.appendChild(head);

          const body = el('tbody');
          for (const item of results) {
            const tr = el('tr');
            tr.appendChild(el('td', '', item.instance_name || item.instance_id || '—'));
            tr.appendChild(el('td', 'port-number', String(portValue(item.port))));
            const protocolCell = el('td');
            protocolCell.appendChild(portProtocolBadge(item.protocol));
            tr.appendChild(protocolCell);
            const stateCell = el('td');
            stateCell.appendChild(el('span', 'status-badge check-state ' + checkStateClass(item.state),
              checkStateLabel(item.state)));
            tr.appendChild(stateCell);
            tr.appendChild(el('td', 'check-detail', item.detail || '—'));
            body.appendChild(tr);
          }
          table.appendChild(body);
          ui.portsCheckResult.appendChild(table);
        }

        const meta = el('p', 'hint ports-check-meta');
        const when = data.checked_at ? formatDate(data.checked_at) : '—';
        const ip = data.public_ip ? data.public_ip : t('ports.no_public_ip', '未配置');
        setText(meta, tf('ports.check_meta', { time: when, ip }, '检测时间：{time} · 公网 IP：{ip}'));
        ui.portsCheckResult.appendChild(meta);
        ui.portsCheckResult.hidden = false;
      }

      async function runPortsCheck() {
        setButtonBusy(ui.checkPorts, true, t('ports.checking', '检测中…'));
        ui.portsCheckLoading.hidden = false;
        ui.portsCheckResult.hidden = true;
        ui.portsCheckNote.hidden = true;
        try {
          const data = await api('/api/ports/check');
          if (!data || !Array.isArray(data.results)) {
            throw new ApiError('E_BAD_RESPONSE', t('errors.bad_response'), '', 0, 0);
          }
          renderPortsCheck(data);
        } catch (err) {
          if (!handleUnauthorized(err)) {
            ui.portsCheckNote.hidden = false;
            ui.portsCheckNote.className = 'alert alert-error';
            setText(ui.portsCheckNote, formatApiError(err));
          }
        } finally {
          ui.portsCheckLoading.hidden = true;
          setButtonBusy(ui.checkPorts, false);
        }
      }

      // ------------------------------------------------------------------- list

      function buildInstanceCard(item) {
        const card = el('article', 'instance-card');

        const main = el('div', 'instance-main');
        const title = el('h3', 'instance-title', item.name || item.id || '');
        main.appendChild(title);
        main.appendChild(el('p', 'instance-sub', item.subdomain || '—'));

        const meta = el('div', 'instance-meta');
        meta.appendChild(el('span', '', tf('dashboard.port_line', {
          voice: portValue(item.ports && item.ports.voice),
          filetransfer: portValue(item.ports && item.ports.filetransfer),
        })));
        if (item.created_at) {
          const created = formatDate(item.created_at);
          if (created) {
            meta.appendChild(el('span', '', tf('dashboard.created_at', { time: created })));
          }
        }
        main.appendChild(meta);

        const side = el('div', 'instance-side');
        const badges = el('div', 'detail-badges');
        const status = el('span', 'status-badge ' + statusClass(item.status), statusLabel(item.status));
        badges.appendChild(status);
        const dns = el('span', 'status-badge ' + dnsClass(item.dns_status), dnsLabel(item.dns_status));
        badges.appendChild(dns);
        side.appendChild(badges);
        const open = button(t('detail.open', '详情'), 'ghost small');
        open.addEventListener('click', () => {
          navigate('#/instances/' + encodeURIComponent(item.id));
        });
        side.appendChild(open);

        card.appendChild(main);
        card.appendChild(side);
        return card;
      }

      function renderInstances(items) {
        let running = 0;
        let pending = 0;
        for (const item of items) {
          if (item.status === 'running' || item.status === 'ready') {
            running += 1;
          }
          if (item.dns_status === 'pending') {
            pending += 1;
          }
        }
        setText(ui.statInstances, items.length);
        setText(ui.statRunning, running);
        setText(ui.statDNS, pending);

        ui.instancesList.replaceChildren();
        if (items.length === 0) {
          ui.instancesEmpty.hidden = false;
          return;
        }
        ui.instancesEmpty.hidden = true;
        for (const item of items) {
          ui.instancesList.appendChild(buildInstanceCard(item));
        }
      }

      async function loadInstances(initial) {
        if (initial) {
          ui.instancesLoading.hidden = false;
          ui.instancesEmpty.hidden = true;
          ui.instancesList.replaceChildren();
        }
        ui.instancesError.hidden = true;
        try {
          const items = await api('/api/instances');
          if (!Array.isArray(items)) {
            throw new ApiError('E_BAD_RESPONSE', t('errors.bad_response'), '', 0, 0);
          }
          renderInstances(items);
        } catch (err) {
          if (handleUnauthorized(err)) {
            return;
          }
          setText(ui.instancesError, formatApiError(err));
          ui.instancesError.hidden = false;
          if (initial) {
            ui.instancesEmpty.hidden = true;
          }
        } finally {
          ui.instancesLoading.hidden = true;
        }
      }

      // ----------------------------------------------------------------- detail

      function setDetailLoading() {
        ui.detailSkeleton.hidden = false;
        ui.detailError.hidden = true;
        ui.detailContent.hidden = true;
      }

      async function loadDetail(id, initial) {
        if (initial) {
          setDetailLoading();
          tokenVisible = false;
          verifyNotice = null;
        }
        const requestID = ++detailRequestID;
        try {
          const detail = await api('/api/instances/' + encodeURIComponent(id));
          if (requestID !== detailRequestID) {
            return;
          }
          currentDetail = detail;
          renderDetail(detail);
          ui.detailSkeleton.hidden = true;
          ui.detailError.hidden = true;
          ui.detailContent.hidden = false;
          if (initial) {
            void loadBackups();
          }
        } catch (err) {
          if (requestID !== detailRequestID) {
            return;
          }
          if (handleUnauthorized(err)) {
            return;
          }
          ui.detailSkeleton.hidden = true;
          ui.detailContent.hidden = true;
          ui.detailError.hidden = false;
          setText(ui.detailErrorMessage, formatApiError(err));
        }
      }

      function renderToken() {
        const token = currentDetail && currentDetail.admin_token ? String(currentDetail.admin_token) : '';
        ui.detailToken.classList.toggle('is-empty', !token);
        ui.tokenToggle.disabled = !token;
        ui.tokenCopy.disabled = !token;
        if (!token) {
          setText(ui.detailToken, t('detail.no_token', '尚未捕获管理员 token'));
          setText(ui.tokenHint, t('detail.no_token_hint', '实例启动后会自动重试抓取；也可以重启实例。'));
          return;
        }
        setText(ui.detailToken, tokenVisible ? token : '••••••••••••');
        setText(ui.tokenHint, tokenVisible
          ? t('detail.token_visible', '请勿在公开场合展示。')
          : t('detail.token_hidden', '默认隐藏，点击“显示”查看完整 token。'));
        setText(ui.tokenToggle, tokenVisible ? t('detail.hide', '隐藏') : t('detail.show', '显示'));
      }

      function buildPortRow(label, port, protocol, scope) {
        const tr = el('tr');
        tr.appendChild(el('td', '', label));
        tr.appendChild(el('td', 'port-number', String(portValue(port))));
        tr.appendChild(el('td', '', protocol));
        const scopeCell = el('td');
        const badge = el('span', 'port-scope' + (scope.local ? ' is-local' : ''), scope.label);
        scopeCell.appendChild(badge);
        tr.appendChild(scopeCell);
        return tr;
      }

      function renderPorts(ports) {
        const table = el('table', 'port-table');
        const head = el('thead');
        const headRow = el('tr');
        for (const label of [
          t('detail.port_use', '用途'),
          t('detail.port_number', '端口'),
          t('detail.port_protocol', '协议'),
          t('detail.port_scope', '范围'),
        ]) {
          headRow.appendChild(el('th', '', label));
        }
        head.appendChild(headRow);
        table.appendChild(head);
        const body = el('tbody');
        body.appendChild(buildPortRow(t('detail.voice', '语音'), ports.voice, 'UDP', {
          local: false,
          label: t('detail.public_scope', '需开放'),
        }));
        body.appendChild(buildPortRow(t('detail.filetransfer', '文件传输'), ports.filetransfer, 'TCP', {
          local: false,
          label: t('detail.public_scope', '需开放'),
        }));
        body.appendChild(buildPortRow(t('detail.query', 'ServerQuery'), ports.query, 'TCP', {
          local: true,
          label: t('detail.local_only', '仅本机'),
        }));
        body.appendChild(buildPortRow(t('detail.query_ssh', 'SSH 查询'), ports.query_ssh, 'TCP', {
          local: true,
          label: t('detail.local_only', '仅本机'),
        }));
        table.appendChild(body);
        ui.detailPorts.replaceChildren(table);
      }

      function connectionAddress(detail) {
        if (isFullDomain(detail.subdomain)) {
          return detail.subdomain;
        }
        if (detail.public_ip) {
          return detail.public_ip + ':' + portValue(detail.ports && detail.ports.voice);
        }
        return '';
      }

      function renderConnection(detail) {
        ui.detailConnection.replaceChildren();
        const rows = [];
        if (isFullDomain(detail.subdomain)) {
          rows.push({
            label: t('detail.domain_address', '域名连接'),
            value: detail.subdomain,
            note: detail.dns_status === 'verified'
              ? t('detail.dns_verified_note', '解析已验证，可直接发给玩家。')
              : t('detail.dns_unverified_note', '解析尚未验证，可先用 IP:端口 连接。'),
          });
        }
        if (detail.public_ip) {
          const address = detail.public_ip + ':' + portValue(detail.ports && detail.ports.voice);
          rows.push({
            label: t('detail.ip_address', 'IP:端口'),
            value: address,
            note: t('detail.ip_note', '域名不可用时直接用这个地址。'),
          });
        }
        if (rows.length === 0) {
          rows.push({
            label: t('detail.address', '连接地址'),
            value: t('detail.not_available', '暂不可用'),
            note: t('detail.address_note', '可在设置页填写公网 IP，或绑定域名。'),
          });
        }
        for (const row of rows) {
          const line = el('div', 'connection-line');
          line.appendChild(el('span', 'connection-label', row.label));
          line.appendChild(el('code', 'connection-value', row.value));
          const copy = button(t('common.copy', '复制'), 'ghost small');
          copy.addEventListener('click', async () => {
            try {
              await copyText(row.value);
              showToastSafe(t('common.copied', '已复制'), 'success');
            } catch (err) {
              showToastSafe(t('common.copy_failed', '复制失败，请手动选择。'), 'error');
            }
          });
          line.appendChild(copy);
          ui.detailConnection.appendChild(line);
          if (row.note) {
            ui.detailConnection.appendChild(el('p', 'connection-note', row.note));
          }
        }
      }

      function metric(label, value, sub, unavailable) {
        const node = el('div', 'metric' + (unavailable ? ' is-unavailable' : ''));
        node.appendChild(el('span', 'metric-label', label));
        node.appendChild(el('strong', 'metric-value', value));
        if (sub) {
          node.appendChild(el('span', 'metric-sub', sub));
        }
        return node;
      }

      function renderStats(detail) {
        const stats = detail.stats || {};
        ui.detailStats.replaceChildren();
        const cpu = formatCPU(stats);
        ui.detailStats.appendChild(metric(
          t('detail.cpu', 'CPU'),
          cpu || t('detail.unavailable', '不可用'),
          cpu ? t('detail.cpu_note', '采样间隔 10 秒') : '',
          !cpu
        ));
        if (stats.available && Number(stats.memory_bytes) >= 0) {
          const used = Number(stats.memory_bytes);
          const limit = Number(stats.memory_limit_bytes);
          ui.detailStats.appendChild(metric(
            t('detail.memory', '内存'),
            limit > 0 ? formatBytes(used) + ' / ' + formatBytes(limit) : formatBytes(used),
            t('detail.memory_note', '工作集内存'),
            false
          ));
        } else {
          ui.detailStats.appendChild(metric(t('detail.memory', '内存'), t('detail.unavailable', '不可用'), '', true));
        }
        ui.detailStats.appendChild(metric(
          t('detail.data_size', '数据目录'),
          formatBytes(detail.data_size_bytes),
          detail.data_dir || '',
          false
        ));
        ui.detailStats.appendChild(metric(
          t('detail.sampled_at', '采样时间'),
          stats.available && stats.sampled_at ? formatDate(stats.sampled_at) : t('detail.unavailable', '不可用'),
          '',
          !stats.available
        ));
      }

      function dnsBadgeText(detail) {
        // dns_status "none" also covers manual mode, where the records are
        // owned by the operator. Showing "未绑定域名" there would hide the
        // manual-mode warning required by errata E9.
        if (detail.dns_manual && detail.dns_status !== 'verified' && detail.dns_status !== 'failed') {
          return t('dns.manual', '手动解析');
        }
        return dnsLabel(detail.dns_status);
      }

      function renderDNS(detail) {
        setText(ui.detailDNSBadge, dnsBadgeText(detail));
        ui.detailDNSBadge.className = 'status-badge ' + dnsClass(detail.dns_status);
        ui.detailDNSManualNote.hidden = !detail.dns_manual;
        ui.detailDNSLine.replaceChildren();

        if (detail.provider) {
          const typeName = providerTypeLabel(detail.provider.type);
          const label = detail.provider.label || detail.provider.id;
          const line = label === typeName
            ? t('detail.provider_line_short', '解析平台：{type} · 主域名 {zone}')
            : t('detail.provider_line', '解析平台：{label}（{type}） · 主域名 {zone}');
          setText(ui.detailDNSLine, format(line, {
            label,
            type: typeName,
            zone: detail.provider.zone || '—',
          }));
        } else {
          setText(ui.detailDNSLine, t('detail.no_provider', '未配置解析平台。可在设置页添加，或使用手动解析清单。'));
        }

        ui.detailDNSDetail.replaceChildren();
        if (detail.subdomain) {
          ui.detailDNSDetail.appendChild(el('span', '', t('detail.subdomain_line', '子域名：') + detail.subdomain));
        }
        if (detail.dns && detail.dns.a_record_id) {
          ui.detailDNSDetail.appendChild(el('span', '', 'A ID: ' + detail.dns.a_record_id));
        }
        if (detail.dns && detail.dns.srv_record_id) {
          ui.detailDNSDetail.appendChild(el('span', '', 'SRV ID: ' + detail.dns.srv_record_id));
        }
        if (detail.dns && detail.dns.verified_at) {
          ui.detailDNSDetail.appendChild(el('span', '', t('detail.verified_at', '验证时间：') + formatDate(detail.dns.verified_at)));
        }

        if (verifyNotice) {
          ui.detailVerifyResult.hidden = false;
          ui.detailVerifyResult.className = 'alert ' + (verifyNotice.ok ? 'alert-success' : 'alert-warning');
          setText(ui.detailVerifyResult, verifyNotice.message);
        } else {
          ui.detailVerifyResult.hidden = true;
          ui.detailVerifyResult.textContent = '';
        }
      }

      function renderDetailActions(detail) {
        const running = detail.status === 'ready' || detail.status === 'running';
        ui.detailStart.disabled = running;
        ui.detailStop.disabled = !running;
        ui.detailRestart.disabled = false;
        const hasDomain = isFullDomain(detail.subdomain);
        ui.verifyDomain.disabled = !hasDomain;
        ui.unbindDomain.disabled = !hasDomain;
      }

      function renderDetail(detail) {
        currentDetail = detail;
        setText(ui.detailName, detail.name || detail.id || '');
        setText(ui.detailSubdomain, detail.subdomain || t('detail.no_subdomain', '未绑定域名'));

        ui.detailBadges.replaceChildren();
        ui.detailBadges.appendChild(el('span', 'status-badge ' + statusClass(detail.status), statusLabel(detail.status)));
        ui.detailBadges.appendChild(el('span', 'status-badge ' + dnsClass(detail.dns_status), dnsBadgeText(detail)));

        renderToken();
        renderConnection(detail);
        renderPorts(detail.ports || {});
        renderStats(detail);
        renderDNS(detail);
        renderDetailActions(detail);

        setText(ui.detailLogMeta, detail.logs_available
          ? tf('detail.logs_meta', { lines: detail.logs_lines }, '最近 {lines} 行 · 已脱敏')
          : '');
        ui.detailLogsFallback.hidden = Boolean(detail.logs_available);
        if (detail.logs_available) {
          setText(ui.detailLogs, detail.logs || t('detail.logs_empty', '暂无日志。'));
        } else {
          setText(ui.detailLogs, '');
        }
      }

      async function runLifecycle(action, buttonNode) {
        if (!currentDetail) {
          return;
        }
        const id = currentDetail.id;
        setButtonBusy(buttonNode, true, t('common.loading', '处理中…'));
        try {
          await api('/api/instances/' + encodeURIComponent(id) + '/' + action, { method: 'POST' });
          showToastSafe(tf('detail.action_ok', { action: action }, '操作已完成。'), 'success');
        } catch (err) {
          if (handleUnauthorized(err)) {
            return;
          }
          showToastSafe(formatApiError(err), 'error');
        } finally {
          setButtonBusy(buttonNode, false);
        }
        await loadDetail(id, false);
      }

      // ------------------------------------------------------------------ modal

      function setModalBody(node) {
        ui.modalBody.replaceChildren(node);
      }

      function openModal(title, node, onClose) {
        lastFocus = document.activeElement;
        setText(ui.modalTitle, title);
        setModalBody(node);
        ui.modalRoot.hidden = false;
        document.body.classList.add('has-modal');
        closeHandler = onClose || null;
        const focusTarget = ui.modalBody.querySelector('input, select, button');
        if (focusTarget) {
          window.setTimeout(() => {
            try {
              focusTarget.focus();
            } catch (err) {
              // ignore
            }
          }, 30);
        }
      }

      function closeModal() {
        if (ui.modalRoot.hidden) {
          return;
        }
        ui.modalRoot.hidden = true;
        document.body.classList.remove('has-modal');
        ui.modalBody.replaceChildren();
        const handler = closeHandler;
        closeHandler = null;
        if (handler) {
          handler();
        }
        if (lastFocus && typeof lastFocus.focus === 'function') {
          try {
            lastFocus.focus();
          } catch (err) {
            // ignore
          }
        }
        lastFocus = null;
      }

      function modalActions(nodes) {
        const actions = el('div', 'actions wrap');
        for (const node of nodes) {
          if (node) {
            actions.appendChild(node);
          }
        }
        return actions;
      }

      function confirmDialog(options) {
        const body = el('div');
        if (options.message) {
          body.appendChild(el('p', '', options.message));
        }
        if (options.extra) {
          body.appendChild(options.extra);
        }
        const cancel = button(t('common.cancel', '取消'), 'ghost');
        cancel.addEventListener('click', closeModal);
        const confirm = button(options.confirmLabel || t('common.delete', '删除'), options.danger === false ? 'primary' : 'danger');
        confirm.addEventListener('click', async () => {
          setButtonBusy(confirm, true, t('common.loading', '处理中…'));
          cancel.disabled = true;
          try {
            await options.onConfirm();
            closeModal();
          } catch (err) {
            setButtonBusy(confirm, false);
            cancel.disabled = false;
            if (!handleUnauthorized(err)) {
              showToastSafe(formatApiError(err), 'error');
            }
          }
        });
        body.appendChild(modalActions([cancel, confirm]));
        openModal(options.title, body);
      }

      // ------------------------------------------------------------ DNS helpers

      function manualTable(records) {
        const table = el('table', 'manual-table');
        const head = el('thead');
        const headRow = el('tr');
        for (const label of [
          t('detail.record_type', '类型'),
          t('detail.record_host', '主机记录'),
          t('detail.record_value', '记录值'),
          '',
        ]) {
          headRow.appendChild(el('th', '', label));
        }
        head.appendChild(headRow);
        table.appendChild(head);
        const body = el('tbody');
        for (const record of records) {
          const tr = el('tr');
          tr.appendChild(el('td', '', record.type));
          tr.appendChild(el('td', '', record.host));
          tr.appendChild(el('td', '', record.value));
          const actionCell = el('td', 'manual-actions');
          const copy = button(t('common.copy', '复制'), 'ghost small');
          copy.addEventListener('click', async () => {
            try {
              await copyText(record.type + '\t' + record.host + '\t' + record.value);
              showToastSafe(t('common.copied', '已复制'), 'success');
            } catch (err) {
              showToastSafe(t('common.copy_failed', '复制失败，请手动选择。'), 'error');
            }
          });
          actionCell.appendChild(copy);
          tr.appendChild(actionCell);
          body.appendChild(tr);
        }
        table.appendChild(body);
        return table;
      }

      function manualText(records) {
        const lines = [t('detail.manual_title', '请在 DNS 控制台添加：'), ''];
        for (const record of records) {
          lines.push(record.type + '\t' + record.host + '\t' + record.value);
        }
        return lines.join('\n');
      }

      function manualPanel(records, onVerify) {
        const wrap = el('div');
        wrap.appendChild(el('p', '', t('detail.manual_intro', '请按下面的记录到 DNS 控制台添加，然后回来验证：')));
        wrap.appendChild(manualTable(records));
        const copyAll = button(t('detail.copy_all', '整段复制'), 'ghost');
        copyAll.addEventListener('click', async () => {
          try {
            await copyText(manualText(records));
            showToastSafe(t('common.copied', '已复制'), 'success');
          } catch (err) {
            showToastSafe(t('common.copy_failed', '复制失败，请手动选择。'), 'error');
          }
        });
        const verify = button(t('detail.added_verify', '我已添加，去验证'), 'primary');
        verify.addEventListener('click', async () => {
          setButtonBusy(verify, true, t('detail.verifying', '验证中…'));
          try {
            await onVerify(verify);
          } finally {
            setButtonBusy(verify, false);
          }
        });
        wrap.appendChild(modalActions([copyAll, verify]));
        return wrap;
      }

      async function openManualChecklist(instanceID) {
        let data;
        try {
          data = await api('/api/instances/' + encodeURIComponent(instanceID) + '/domain/manual');
        } catch (err) {
          if (!handleUnauthorized(err)) {
            showToastSafe(formatApiError(err), 'error');
          }
          return;
        }
        if (!data || !Array.isArray(data.records) || data.records.length === 0) {
          showToastSafe(t('detail.manual_empty', '无法生成手动解析清单，请先配置解析平台和公网 IP。'), 'error');
          return;
        }
        const body = el('div');
        body.appendChild(el('p', 'muted', tf('detail.manual_zone', { zone: data.zone || '—' }, '主域名：{zone}')));
        body.appendChild(manualPanel(data.records, async (btn) => {
          try {
            const result = await api('/api/instances/' + encodeURIComponent(instanceID) + '/domain/verify', { method: 'POST' });
            if (result && result.verified) {
              showToastSafe(t('detail.verify_ok', '解析已验证。'), 'success');
              closeModal();
              await loadDetail(instanceID, false);
            } else {
              showToastSafe(result && result.message ? result.message : t('detail.verify_pending', '记录仍在传播，请稍后再试。'), 'error');
            }
          } catch (err) {
            if (!handleUnauthorized(err)) {
              showToastSafe(formatApiError(err), 'error');
            }
          }
          setButtonBusy(btn, false);
        }));
        openModal(t('detail.manual_title', '手动解析清单'), body);
      }

      // ------------------------------------------------------------- wizard core

      function makeProgress(container, labels) {
        container.replaceChildren();
        container.appendChild(el('p', 'muted wizard-intro', t('create.progress_title', '正在创建实例，请保持页面打开。')));

        const stepper = el('ol', 'stepper');
        const stepNodes = [];
        labels.forEach((label, index) => {
          const li = el('li', 'step');
          li.dataset.step = String(index + 1);
          li.appendChild(el('span', 'step-number', String(index + 1)));
          li.appendChild(el('span', 'step-label', label));
          stepper.appendChild(li);
          stepNodes.push(li);
        });
        container.appendChild(stepper);

        const statusList = el('ul', 'wizard-status');
        const statusNodes = labels.map(() => {
          const li = el('li', '', '');
          statusList.appendChild(li);
          return li;
        });
        container.appendChild(statusList);

        const alertBox = el('div', 'alert alert-error');
        alertBox.hidden = true;
        container.appendChild(alertBox);

        return {
          setStep(index, state, note) {
            const node = stepNodes[index];
            if (!node) {
              return;
            }
            node.classList.toggle('is-active', state === 'active');
            node.classList.toggle('is-done', state === 'done');
            node.classList.toggle('is-failed', state === 'failed');
            node.classList.toggle('is-skipped', state === 'skipped');
            if (state === 'active') {
              node.setAttribute('aria-current', 'step');
            } else {
              node.removeAttribute('aria-current');
            }
            const status = statusNodes[index];
            if (status) {
              status.className = state ? 'is-' + state : '';
              status.textContent = note || '';
            }
          },
          setError(message) {
            alertBox.hidden = !message;
            setText(alertBox, message || '');
          },
        };
      }

      function progressStages() {
        return [
          t('create.step_container', '建容器'),
          t('create.step_token', '抓密钥'),
          t('create.step_dns', '写解析'),
          t('create.step_verify', '验证'),
        ];
      }

      function failureDetails(source) {
        if (source && source.payload) {
          return {
            code: source.code || 'E_DNS_UNKNOWN',
            message: source.message || t('errors.unknown'),
            hint: source.hint || '',
            manual: Array.isArray(source.payload.manual) ? source.payload.manual : [],
            retryable: source.payload.retryable !== false,
          };
        }
        if (source && source.error && source.error.code) {
          return {
            code: source.error.code,
            message: source.error.message || '',
            hint: source.error.hint || '',
            manual: Array.isArray(source.manual) ? source.manual : [],
            retryable: source.retryable !== false,
          };
        }
        if (source && source.manual) {
          return {
            code: source.code || 'E_DNS_PENDING',
            message: source.message || '',
            hint: source.hint || '',
            manual: Array.isArray(source.manual) ? source.manual : [],
            retryable: true,
          };
        }
        return {
          code: (source && source.code) || 'E_DNS_UNKNOWN',
          message: (source && source.message) || t('errors.unknown'),
          hint: (source && source.hint) || '',
          manual: [],
          retryable: true,
        };
      }

      function renderFailure(container, options, handlers) {
        const failure = failureDetails(options.error);
        const wrap = el('div');
        wrap.appendChild(el('p', '', t('create.failed_intro', '实例已正常运行，域名绑定没有完成。可重试，或按下面的清单手动添加记录。')));
        wrap.appendChild(el('p', 'failure-code', failure.code));
        wrap.appendChild(el('p', '', failure.message));
        if (failure.hint) {
          wrap.appendChild(el('p', 'muted', failure.hint));
        }

        if (failure.manual.length > 0) {
          wrap.appendChild(manualTable(failure.manual));
          const copyAll = button(t('detail.copy_all', '整段复制'), 'ghost');
          copyAll.addEventListener('click', async () => {
            try {
              await copyText(manualText(failure.manual));
              showToastSafe(t('common.copied', '已复制'), 'success');
            } catch (err) {
              showToastSafe(t('common.copy_failed', '复制失败，请手动选择。'), 'error');
            }
          });
          const verify = button(t('detail.added_verify', '我已添加，去验证'), 'primary');
          verify.addEventListener('click', async () => {
            setButtonBusy(verify, true, t('detail.verifying', '验证中…'));
            try {
              await handlers.onVerify(verify);
            } finally {
              setButtonBusy(verify, false);
            }
          });
          wrap.appendChild(modalActions([copyAll, verify]));
        } else {
          const manual = button(t('detail.manual_checklist', '手动解析清单'), 'ghost');
          manual.addEventListener('click', () => {
            void openManualChecklist(options.instanceID);
          });
          wrap.appendChild(modalActions([manual]));
        }

        if (failure.retryable && handlers.onRetry) {
          const retry = button(t('create.retry', '重试自动绑定'), 'ghost');
          retry.addEventListener('click', () => {
            handlers.onRetry();
          });
          wrap.appendChild(modalActions([retry]));
        }

        const close = button(t('common.close', '关闭'), 'primary');
        close.addEventListener('click', closeModal);
        wrap.appendChild(modalActions([close]));

        container.replaceChildren(wrap);
      }

      function successPanel(options) {
        const wrap = el('div');
        wrap.appendChild(el('p', '', options.message || t('create.success_body', '实例已创建并保存。')));
        const connection = el('div', 'connection-box');
        const line = el('div', 'connection-line');
        line.appendChild(el('span', 'connection-label', t('detail.address', '连接地址')));
        line.appendChild(el('code', 'connection-value', options.address));
        const copy = button(t('common.copy', '复制'), 'ghost small');
        copy.addEventListener('click', async () => {
          try {
            await copyText(options.address);
            showToastSafe(t('common.copied', '已复制'), 'success');
          } catch (err) {
            showToastSafe(t('common.copy_failed', '复制失败，请手动选择。'), 'error');
          }
        });
        line.appendChild(copy);
        connection.appendChild(line);
        if (options.note) {
          connection.appendChild(el('p', 'connection-note', options.note));
        }
        wrap.appendChild(connection);
        if (options.token) {
          wrap.appendChild(el('p', 'muted', t('create.token_saved', '管理员 token 已保存，可在实例详情页查看和复制。')));
        }
        const view = button(t('create.view_detail', '查看实例详情'), 'primary');
        view.addEventListener('click', () => {
          closeModal();
          navigate('#/instances/' + encodeURIComponent(options.instanceID));
        });
        const done = button(t('common.done', '完成'), 'ghost');
        done.addEventListener('click', () => {
          closeModal();
          void loadInstances(false);
        });
        wrap.appendChild(modalActions([view, done]));
        return wrap;
      }

      // ------------------------------------------------------------- domain form

      function buildProviderSelect(selectedID) {
        const select = el('select');
        const none = el('option', '', t('create.no_provider_option', '暂不绑定（稍后设置）'));
        none.value = '';
        select.appendChild(none);
        for (const provider of providers) {
          const option = el('option', '', provider.label + ' · ' + provider.zone +
            (provider.type === 'manual' ? ' ' + t('create.manual_suffix', '（手动）') : ''));
          option.value = provider.id;
          select.appendChild(option);
        }
        select.value = selectedID || '';
        return select;
      }

      function buildDomainForm(options) {
        const wrap = el('div');
        if (options.intro) {
          wrap.appendChild(el('p', 'muted', options.intro));
        }

        const providerField = el('div', 'field');
        const providerLabel = el('label', '', t('create.provider', '解析平台'));
        const providerSelect = buildProviderSelect(options.providerID);
        providerLabel.htmlFor = 'domain-provider';
        providerSelect.id = 'domain-provider';
        providerField.appendChild(providerLabel);
        providerField.appendChild(providerSelect);
        providerField.appendChild(el('p', 'hint', providers.length === 0
          ? t('create.no_providers_hint', '尚未配置解析平台，可先创建实例，稍后在设置页添加。')
          : t('create.provider_hint', '选择“暂不绑定”也可以先按 IP:端口 使用。')));
        wrap.appendChild(providerField);

        const prefixInput = textInput(options.prefix || '', 'ts1', 20);
        prefixInput.autocomplete = 'off';
        prefixInput.spellcheck = false;
        wrap.appendChild(field(
          t('create.prefix', '子域名前缀'),
          prefixInput,
          t('create.prefix_hint', '1–20 位小写字母、数字或连字符，例如 ts1。实例 ID 创建后不可修改。'),
        ));

        const preview = el('p', 'preview-line', '');
        wrap.appendChild(preview);

        const checkButton = button(t('create.check', '检查冲突'), 'ghost small');
        const checkRow = el('div', 'actions wrap');
        checkRow.appendChild(checkButton);
        wrap.appendChild(checkRow);

        const checkResult = el('div', 'alert');
        checkResult.hidden = true;
        wrap.appendChild(checkResult);

        const overwriteRow = el('label', 'option-row');
        overwriteRow.hidden = true;
        const overwriteInput = el('input');
        overwriteInput.type = 'checkbox';
        overwriteRow.appendChild(overwriteInput);
        overwriteRow.appendChild(el('span', '', t('create.overwrite_label', '我了解继续会覆盖该子域名下已有的 A / SRV 记录。')));
        wrap.appendChild(overwriteRow);

        const errorBox = el('div', 'alert alert-error');
        errorBox.hidden = true;
        wrap.appendChild(errorBox);

        const submit = button(options.submitLabel, 'primary');
        const back = options.onBack ? button(t('common.back', '上一步'), 'ghost') : null;
        if (back) {
          back.addEventListener('click', options.onBack);
        }
        wrap.appendChild(modalActions([back, submit]));

        const state = { precheck: null, checking: false, checkedPrefix: '', checkedProvider: '', conflict: false };
        let debounce = null;
        let requestSeq = 0;

        function currentProvider() {
          return findProvider(providerSelect.value);
        }

        function updatePreview() {
          const prefix = prefixInput.value.trim().toLowerCase();
          const provider = currentProvider();
          if (!prefix) {
            setText(preview, t('create.preview_empty', '填写前缀后显示完整子域名。'));
            return;
          }
          if (provider && provider.zone) {
            setText(preview, prefix + '.' + provider.zone);
          } else {
            setText(preview, prefix);
          }
        }

        function applyPrecheck(result) {
          checkResult.hidden = false;
          checkResult.replaceChildren();
          checkResult.className = 'alert';
          overwriteRow.hidden = true;
          overwriteInput.checked = false;
          state.precheck = result;
          submit.disabled = false;

          if (!result) {
            checkResult.className = 'alert alert-warning';
            setText(checkResult, t('create.check_failed', '无法完成冲突检查，创建时服务端仍会检查。'));
            return;
          }
          if (result.warning) {
            checkResult.className = 'alert alert-warning';
            setText(checkResult, result.warning);
            return;
          }
          if (result.checked === false) {
            // Manual mode and "no provider" both return a message without a real
            // record lookup; this is information, not a successful check.
            checkResult.className = 'alert alert-warning';
            setText(checkResult, result.message || t('create.check_skipped', '已跳过现有记录检查。'));
            return;
          }
          if (result.local_conflict) {
            checkResult.className = 'alert alert-error';
            setText(checkResult, result.message || t('create.local_conflict', '该子域名已被其他实例占用。'));
            submit.disabled = true;
            return;
          }
          if (!result.available && Array.isArray(result.conflicts) && result.conflicts.length > 0) {
            checkResult.className = 'alert alert-warning';
            setText(checkResult, result.message || t('create.record_conflict', '该子域名已有 A / SRV 记录。'));
            overwriteRow.hidden = false;
            submit.disabled = true;
            return;
          }
          checkResult.className = 'alert alert-success';
          setText(checkResult, result.message || t('create.available', '未发现冲突，可以创建。'));
        }

        function validatePrefix(prefix) {
          return /^[a-z0-9](?:[a-z0-9-]{0,18}[a-z0-9])?$/.test(prefix);
        }

        async function runCheck() {
          const prefix = prefixInput.value.trim().toLowerCase();
          if (!prefix) {
            checkResult.hidden = true;
            state.precheck = null;
            submit.disabled = false;
            return null;
          }
          if (!validatePrefix(prefix)) {
            checkResult.hidden = false;
            checkResult.className = 'alert alert-error';
            setText(checkResult, t('create.prefix_invalid', '前缀只能使用小写字母、数字和连字符，且不能以连字符开头或结尾。'));
            submit.disabled = true;
            return null;
          }
          const seq = ++requestSeq;
          state.checking = true;
          state.checkedPrefix = prefix;
          state.checkedProvider = providerSelect.value;
          checkResult.hidden = false;
          checkResult.className = 'alert alert-warning';
          setText(checkResult, t('create.checking', '正在检查冲突…'));
          updatePreview();
          try {
            const params = new URLSearchParams({ subdomain: prefix });
            if (providerSelect.value) {
              params.set('provider_id', providerSelect.value);
            }
            if (options.instanceID) {
              params.set('instance_id', options.instanceID);
            }
            const result = await api('/api/domain/precheck?' + params.toString());
            if (seq !== requestSeq) {
              return null;
            }
            state.checking = false;
            state.conflict = Boolean(result && !result.available);
            applyPrecheck(result);
            updateSubmitState();
            return result;
          } catch (err) {
            if (seq !== requestSeq) {
              return null;
            }
            state.checking = false;
            state.precheck = null;
            state.conflict = false;
            overwriteRow.hidden = true;
            submit.disabled = false;
            checkResult.hidden = false;
            checkResult.className = 'alert alert-warning';
            setText(checkResult, t('create.check_failed', '无法完成冲突检查，创建时服务端仍会检查。'));
            return null;
          }
        }

        function updateSubmitState() {
          if (state.checking) {
            submit.disabled = true;
            return;
          }
          if (state.precheck && state.precheck.local_conflict) {
            submit.disabled = true;
            return;
          }
          if (state.precheck && !state.precheck.available && !state.precheck.local_conflict &&
              Array.isArray(state.precheck.conflicts) && state.precheck.conflicts.length > 0) {
            submit.disabled = !overwriteInput.checked;
            return;
          }
          submit.disabled = false;
        }

        overwriteInput.addEventListener('change', updateSubmitState);
        providerSelect.addEventListener('change', () => {
          updatePreview();
          if (debounce !== null) {
            window.clearTimeout(debounce);
          }
          debounce = window.setTimeout(() => {
            void runCheck();
          }, 250);
        });
        prefixInput.addEventListener('input', () => {
          updatePreview();
          if (debounce !== null) {
            window.clearTimeout(debounce);
          }
          debounce = window.setTimeout(() => {
            void runCheck();
          }, 450);
        });
        checkButton.addEventListener('click', () => {
          void runCheck();
        });

        submit.addEventListener('click', async () => {
          errorBox.hidden = true;
          const prefix = prefixInput.value.trim().toLowerCase();
          if (!validatePrefix(prefix)) {
            errorBox.hidden = false;
            setText(errorBox, t('create.prefix_invalid', '前缀只能使用小写字母、数字和连字符，且不能以连字符开头或结尾。'));
            prefixInput.focus();
            return;
          }
          if (state.checking || !state.precheck ||
              state.checkedPrefix !== prefix || state.checkedProvider !== providerSelect.value) {
            await runCheck();
          }
          if (state.precheck && state.precheck.local_conflict) {
            errorBox.hidden = false;
            setText(errorBox, state.precheck.message || t('create.local_conflict', '该子域名已被其他实例占用。'));
            return;
          }
          const conflicting = state.precheck && !state.precheck.available &&
            Array.isArray(state.precheck.conflicts) && state.precheck.conflicts.length > 0;
          if (conflicting && !overwriteInput.checked) {
            errorBox.hidden = false;
            setText(errorBox, t('create.overwrite_required', '请勾选确认覆盖，或换一个前缀。'));
            return;
          }
          const provider = currentProvider();
          options.onSubmit({
            prefix,
            providerID: providerSelect.value,
            provider,
            overwrite: Boolean(conflicting && overwriteInput.checked),
          }, { submit });
        });

        updatePreview();
        if (prefixInput.value) {
          window.setTimeout(() => {
            void runCheck();
          }, 0);
        }
        return { node: wrap, runCheck, prefixInput, providerSelect };
      }

      function showFailureModal(error, context, handlers) {
        const body = el('div');
        renderFailure(body, { error, instanceID: context.instanceID }, handlers);
        openModal(t('create.failed_title', '域名绑定失败'), body, () => {
          if (currentView === 'detail' && currentDetailID) {
            void loadDetail(currentDetailID, false);
          } else {
            void loadInstances(false);
          }
        });
      }

      // ---------------------------------------------------------- create wizard

      async function openCreateWizard() {
        const state = { name: '', prefix: '', providerID: '', provider: null, overwrite: false };

        await ensureProviders();
      if (!state.providerID && providers.length > 0) {
        const preferred = providers.find((item) => item.default) || providers[0];
        if (preferred) {
          state.providerID = preferred.id;
        }
      }

        function stepOne() {
          const body = el('div');
          body.appendChild(el('p', 'muted', t('create.step1_desc', '给实例起一个名字。下一步选择解析平台并填写子域名。')));
          const nameInput = textInput(state.name, t('create.name_placeholder', '例如 一队'), 40);
          body.appendChild(field(t('create.name', '实例名称'), nameInput, t('create.name_hint', '1–40 个字符，仅用于面板显示。')));
          const error = el('div', 'alert alert-error');
          error.hidden = true;
          body.appendChild(error);
          const cancel = button(t('common.cancel', '取消'), 'ghost');
          cancel.addEventListener('click', closeModal);
          const next = button(t('common.next', '下一步'), 'primary');
          next.addEventListener('click', () => {
            const name = nameInput.value.trim();
            if (!name) {
              error.hidden = false;
              setText(error, t('create.name_required', '请填写实例名称。'));
              nameInput.focus();
              return;
            }
            state.name = name;
            stepTwo();
          });
          nameInput.addEventListener('keydown', (event) => {
            if (event.key === 'Enter') {
              event.preventDefault();
              next.click();
            }
          });
          body.appendChild(modalActions([cancel, next]));
          openModal(t('create.title', '新建实例'), body);
        }

        function stepTwo() {
          const body = el('div');
          const form = buildDomainForm({
            intro: t('create.step2_desc', '选择解析平台并填写子域名。没有解析平台时可以先跳过域名绑定。'),
            prefix: state.prefix,
            providerID: state.providerID,
            submitLabel: t('create.submit', '创建实例'),
            onBack: stepOne,
            onSubmit: (values, uiRefs) => {
              state.prefix = values.prefix;
              state.providerID = values.providerID;
              state.provider = values.provider;
              state.overwrite = values.overwrite;
              void runCreate(state, uiRefs.submit);
            },
          });
          body.appendChild(form.node);
          openModal(t('create.title', '新建实例'), body);
          window.setTimeout(() => {
            form.prefixInput.focus();
          }, 30);
        }

        function runCreate(current, submitButton) {
          setButtonBusy(submitButton, true, t('create.creating', '创建中…'));
          const progressBody = el('div');
          setModalBody(progressBody);
          const progress = makeProgress(progressBody, progressStages());

          const instanceLabel = current.name;
          void (async () => {
            current.providerID = current.providerID || '';
            progress.setStep(0, 'active', t('create.container_starting', '正在创建容器并启动 TS3（可能需要拉取镜像）…'));
            let created;
            try {
              created = await api('/api/instances', {
                method: 'POST',
                body: {
                  name: current.name,
                  subdomain: current.prefix,
                  provider_id: current.providerID || undefined,
                },
              });
            } catch (err) {
              progress.setStep(0, 'failed', formatApiError(err));
              progress.setError(formatApiError(err));
              setButtonBusy(submitButton, false);
              return;
            }

            progress.setStep(0, 'done', t('create.container_done', '容器已启动。'));
            if (created.admin_token) {
              progress.setStep(1, 'done', t('create.token_done', '已捕获管理员 token。'));
            } else {
              progress.setStep(1, 'done', t('create.token_missing', '暂未捕获 token，可在详情页重启重试。'));
            }

            if (ui.modalRoot.hidden) {
              showToastSafe(tf('create.created_background', { name: instanceLabel }, '实例「{name}」已创建。'), 'success');
              void loadInstances(false);
              return;
            }

            if (!current.providerID || !current.provider) {
              progress.setStep(2, 'skipped', t('create.dns_skipped', '未选择解析平台，已跳过。'));
              progress.setStep(3, 'skipped', t('create.verify_skipped', '未绑定域名，已跳过。'));
              showCreated(created);
              setButtonBusy(submitButton, false);
              return;
            }

            await runBinding(created, current, progress, progressBody);
            setButtonBusy(submitButton, false);
          })();
        }

        async function runBinding(created, current, progress, progressBody) {
          progress.setStep(2, 'active', t('create.dns_starting', '正在写入 A 与 SRV 记录…'));
          let bind;
          try {
            bind = await api('/api/instances/' + encodeURIComponent(created.id) + '/domain', {
              method: 'POST',
              body: {
                subdomain: current.prefix,
                provider_id: current.providerID,
                overwrite: current.overwrite,
                verify: false,
              },
            });
          } catch (err) {
            progress.setStep(2, 'failed', formatApiError(err));
            if (!ui.modalRoot.hidden) {
              showFailureModal(err, { instanceID: created.id }, {
                onRetry: () => {
                  const progressBody2 = el('div');
                  setModalTitleText(t('create.progress_title', '正在创建实例，请保持页面打开。'));
                  setModalBody(progressBody2);
                  const progress2 = makeProgress(progressBody2, progressStages());
                  progress2.setStep(0, 'done', t('create.container_done', '容器已启动。'));
                  progress2.setStep(1, 'done', created.admin_token
                    ? t('create.token_done', '已捕获管理员 token。')
                    : t('create.token_missing', '暂未捕获 token，可在详情页重启重试。'));
                  void runBinding(created, current, progress2, progressBody2);
                },
                onVerify: (buttonNode) => verifyInstanceDomain(created.id, buttonNode, () => {
                  closeModal();
                  void loadInstances(false);
                }),
              });
            } else {
              showToastSafe(formatApiError(err), 'error');
            }
            return;
          }

          if (bind.dns_status === 'none') {
            progress.setStep(2, 'done', t('create.manual_ready', '手动解析清单已生成。'));
            progress.setStep(3, 'active', t('create.manual_wait', '请在 DNS 控制台添加记录后点“我已添加，去验证”。'));
            if (!ui.modalRoot.hidden) {
              showManualStep(created, bind.manual || [], progressBody);
            }
            return;
          }

          progress.setStep(2, 'done', t('create.dns_done', 'A 与 SRV 记录已写入。'));
          progress.setStep(3, 'active', t('create.verify_starting', '正在验证解析生效（最长 180 秒）…'));

          let result;
          try {
            result = await api('/api/instances/' + encodeURIComponent(created.id) + '/domain/verify', { method: 'POST' });
          } catch (err) {
            progress.setStep(3, 'failed', formatApiError(err));
            if (!ui.modalRoot.hidden) {
              showFailureModal(err, { instanceID: created.id }, {
                onRetry: () => retryVerify(created, current, progressBody),
                onVerify: (buttonNode) => verifyInstanceDomain(created.id, buttonNode, () => {
                  closeModal();
                  void loadInstances(false);
                }),
              });
            }
            return;
          }

          if (result && result.verified) {
            progress.setStep(3, 'done', t('create.verify_done', '解析已验证。'));
            showCreated(created);
            return;
          }

          progress.setStep(3, 'failed', result && result.message ? result.message : t('create.verify_pending', '解析尚未生效。'));
          if (!ui.modalRoot.hidden) {
            showFailureModal(result, { instanceID: created.id }, {
              onRetry: () => retryVerify(created, current, progressBody),
              onVerify: (buttonNode) => verifyInstanceDomain(created.id, buttonNode, () => {
                closeModal();
                void loadInstances(false);
              }),
            });
          }
        }

        function retryVerify(created, current, progressBody) {
          const freshBody = el('div');
          setModalTitleText(t('create.progress_title', '正在创建实例，请保持页面打开。'));
          setModalBody(freshBody);
          const fresh = makeProgress(freshBody, progressStages());
          fresh.setStep(0, 'done', t('create.container_done', '容器已启动。'));
          fresh.setStep(1, 'done', created.admin_token
            ? t('create.token_done', '已捕获管理员 token。')
            : t('create.token_missing', '暂未捕获 token，可在详情页重启重试。'));
          fresh.setStep(2, 'done', t('create.dns_done', 'A 与 SRV 记录已写入。'));
          fresh.setStep(3, 'active', t('create.verify_starting', '正在验证解析生效（最长 180 秒）…'));
          void (async () => {
            try {
              const result = await api('/api/instances/' + encodeURIComponent(created.id) + '/domain/verify', { method: 'POST' });
              if (result && result.verified) {
                fresh.setStep(3, 'done', t('create.verify_done', '解析已验证。'));
                showCreated(created);
              } else {
                fresh.setStep(3, 'failed', result && result.message ? result.message : t('create.verify_pending', '解析尚未生效。'));
                showFailureModal(result, { instanceID: created.id }, {
                  onRetry: () => retryVerify(created, current, progressBody),
                  onVerify: (buttonNode) => verifyInstanceDomain(created.id, buttonNode, () => {
                    closeModal();
                    void loadInstances(false);
                  }),
                });
              }
            } catch (err) {
              fresh.setStep(3, 'failed', formatApiError(err));
              showFailureModal(err, { instanceID: created.id }, {
                onRetry: () => retryVerify(created, current, progressBody),
              });
            }
          })();
        }

        function showManualStep(created, records, progressBody) {
          const body = el('div');
          body.appendChild(el('p', '', t('create.manual_body', '实例已创建完成。请按清单到 DNS 控制台添加记录，然后回来验证。')));
          body.appendChild(manualPanel(records, async (buttonNode) => {
            try {
              const result = await api('/api/instances/' + encodeURIComponent(created.id) + '/domain/verify', { method: 'POST' });
              if (result && result.verified) {
                showToastSafe(t('detail.verify_ok', '解析已验证。'), 'success');
                showCreated(created);
              } else {
                showToastSafe(result && result.message ? result.message : t('detail.verify_pending', '记录仍在传播，请稍后再试。'), 'error');
              }
            } catch (err) {
              if (!handleUnauthorized(err)) {
                showToastSafe(formatApiError(err), 'error');
              }
            }
            setButtonBusy(buttonNode, false);
          }));
          body.appendChild(modalActions([
            (() => {
              const done = button(t('common.done', '完成'), 'ghost');
              done.addEventListener('click', () => {
                closeModal();
                void loadInstances(false);
              });
              return done;
            })(),
          ]));
          setModalBody(body);
        }

        function showCreated(created) {
          let address = created.subdomain || created.id;
          let note = t('create.success_bound', '域名绑定完成。');
          if (!isFullDomain(created.subdomain)) {
            if (publicIP) {
              address = publicIP + ':' + portValue(created.ports && created.ports.voice);
              note = t('create.success_ip', '尚未绑定域名，请先用 IP:端口 连接。');
            } else {
              address = t('create.success_no_ip', '请到设置页填写服务器公网 IP 后连接。');
              note = t('create.success_no_ip_note', '实例已运行，填写公网 IP 后可按 IP:端口 连接。');
            }
          }
          const body = successPanel({
            instanceID: created.id,
            address,
            note,
            token: created.admin_token,
          });
          setModalTitleText(t('create.success_title', '实例已创建'));
          setModalBody(body);
          closeHandler = () => {
            void loadInstances(false);
          };
        }

        stepOne();
      }

      function setModalTitleText(text) {
        setText(ui.modalTitle, text);
      }

      async function verifyInstanceDomain(instanceID, buttonNode, onVerified) {
        setButtonBusy(buttonNode, true, t('detail.verifying', '验证中…'));
        try {
          const result = await api('/api/instances/' + encodeURIComponent(instanceID) + '/domain/verify', { method: 'POST' });
          if (result && result.verified) {
            showToastSafe(t('detail.verify_ok', '解析已验证。'), 'success');
            if (onVerified) {
              onVerified();
            }
          } else {
            showToastSafe(result && result.message ? result.message : t('detail.verify_pending', '记录仍在传播，请稍后再试。'), 'error');
          }
          return result;
        } catch (err) {
          if (!handleUnauthorized(err)) {
            showToastSafe(formatApiError(err), 'error');
          }
          return null;
        } finally {
          setButtonBusy(buttonNode, false);
        }
      }

      // ------------------------------------------------------------- bind modal

      function openBindModal(detail) {
        if (providers.length === 0) {
          const body = el('div');
          body.appendChild(el('p', '', t('detail.no_provider_bind', '尚未配置解析平台。请先在设置页添加平台，或使用手动解析模式。')));
          const settings = button(t('nav.settings', '设置'), 'primary');
          settings.addEventListener('click', () => {
            closeModal();
            navigate('#/settings');
          });
          body.appendChild(modalActions([settings]));
          openModal(t('detail.bind_domain', '绑定 / 更换域名'), body);
          return;
        }

        const currentProvider = detail.provider_id || (providers.find((p) => p.default) || providers[0] || {}).id || '';
        let prefix = '';
        if (isFullDomain(detail.subdomain)) {
          prefix = detail.subdomain.split('.')[0];
        }
        const body = el('div');

        const manualWarning = el('div', 'alert alert-warning');
        manualWarning.hidden = true;
        manualWarning.textContent = t('detail.manual_switch_warning',
          '切换到手动解析后，面板之前自动创建的 A / SRV 记录会被删除；手动添加的记录需要你自己去 DNS 控制台删除。');
        body.appendChild(manualWarning);

        const form = buildDomainForm({
          intro: t('detail.bind_intro', '更换子域名会先删除面板创建的旧记录，再写入新记录。'),
          prefix,
          providerID: currentProvider,
          instanceID: detail.id,
          submitLabel: t('detail.bind_submit', '开始绑定'),
          onSubmit: (values) => {
            void runBindFlow(detail, values);
          },
        });
        body.appendChild(form.node);

        const providerSelect = form.providerSelect;
        function syncManualWarning() {
          const provider = findProvider(providerSelect.value);
          const hadAutoRecords = Boolean(detail.dns && (detail.dns.a_record_id || detail.dns.srv_record_id));
          manualWarning.hidden = !(provider && provider.type === 'manual' && hadAutoRecords);
        }
        providerSelect.addEventListener('change', syncManualWarning);
        syncManualWarning();
        openModal(t('detail.bind_domain', '绑定 / 更换域名'), body);
      }

      function runBindFlow(detail, values) {
        const body = el('div');
        setModalBody(body);
        const progress = makeProgress(body, progressStages());
        progress.setStep(0, 'done', t('create.container_exists', '容器已存在。'));
        progress.setStep(1, detail.has_token
          ? 'done'
          : 'done', detail.has_token
          ? t('create.token_done', '已捕获管理员 token。')
          : t('create.token_missing', '暂未捕获 token，可在详情页重启重试。'));

        const flow = {
          prefix: values.prefix,
          providerID: values.providerID,
          provider: values.provider,
          overwrite: values.overwrite,
        };

        void (async () => {
          progress.setStep(2, 'active', t('create.dns_starting', '正在写入 A 与 SRV 记录…'));
          let bind;
          try {
            bind = await api('/api/instances/' + encodeURIComponent(detail.id) + '/domain', {
              method: 'POST',
              body: {
                subdomain: flow.prefix,
                provider_id: flow.providerID,
                overwrite: flow.overwrite,
                verify: false,
              },
            });
          } catch (err) {
            progress.setStep(2, 'failed', formatApiError(err));
            showFailureModal(err, { instanceID: detail.id }, {
              onRetry: () => runBindFlow(detail, values),
              onVerify: (buttonNode) => verifyInstanceDomain(detail.id, buttonNode, () => {
                closeModal();
                void loadDetail(detail.id, false);
              }),
            });
            return;
          }

          if (bind.dns_status === 'none') {
            progress.setStep(2, 'done', t('create.manual_ready', '手动解析清单已生成。'));
            progress.setStep(3, 'active', t('create.manual_wait', '请在 DNS 控制台添加记录后点“我已添加，去验证”。'));
            const manualBody = el('div');
            manualBody.appendChild(el('p', '', t('detail.manual_body_bind', '请按清单到 DNS 控制台添加记录，然后回来验证。')));
            manualBody.appendChild(manualPanel(bind.manual || [], async (buttonNode) => {
              await verifyInstanceDomain(detail.id, buttonNode, () => {
                closeModal();
                void loadDetail(detail.id, false);
              });
              setButtonBusy(buttonNode, false);
            }));
            const done = button(t('common.done', '完成'), 'ghost');
            done.addEventListener('click', () => {
              closeModal();
              void loadDetail(detail.id, false);
            });
            manualBody.appendChild(modalActions([done]));
            body.replaceChildren(manualBody);
            return;
          }

          progress.setStep(2, 'done', t('create.dns_done', 'A 与 SRV 记录已写入。'));
          progress.setStep(3, 'active', t('create.verify_starting', '正在验证解析生效（最长 180 秒）…'));
          let result;
          try {
            result = await api('/api/instances/' + encodeURIComponent(detail.id) + '/domain/verify', { method: 'POST' });
          } catch (err) {
            progress.setStep(3, 'failed', formatApiError(err));
            showFailureModal(err, { instanceID: detail.id }, {
              onRetry: () => runBindFlow(detail, values),
              onVerify: (buttonNode) => verifyInstanceDomain(detail.id, buttonNode, () => {
                closeModal();
                void loadDetail(detail.id, false);
              }),
            });
            return;
          }

          if (result && result.verified) {
            progress.setStep(3, 'done', t('create.verify_done', '解析已验证。'));
            showToastSafe(t('detail.verify_ok', '解析已验证。'), 'success');
            closeModal();
            await loadDetail(detail.id, false);
            return;
          }

          progress.setStep(3, 'failed', result && result.message ? result.message : t('create.verify_pending', '解析尚未生效。'));
          showFailureModal(result, { instanceID: detail.id }, {
            onRetry: () => runBindFlow(detail, values),
            onVerify: (buttonNode) => verifyInstanceDomain(detail.id, buttonNode, () => {
              closeModal();
              void loadDetail(detail.id, false);
            }),
          });
        })();
      }

      // ---------------------------------------------------------------- providers

      async function ensureProviders() {
        if (providers.length > 0) {
          return;
        }
        try {
          await loadProviders(false);
        } catch (err) {
          // Settings and the wizard can still work with an empty list.
        }
      }

      function loadProviders(showLoading) {
        if (providersRequest) {
          return providersRequest;
        }
        if (showLoading && ui.providersLoading) {
          ui.providersLoading.hidden = false;
        }
        if (ui.providersError) {
          ui.providersError.hidden = true;
        }
        providersRequest = (async () => {
          try {
            const list = await api('/api/providers');
            if (!Array.isArray(list)) {
              throw new ApiError('E_BAD_RESPONSE', t('errors.bad_response'), '', 0, 0);
            }
            providers = list;
            renderProviders();
          } catch (err) {
            if (handleUnauthorized(err)) {
              return;
            }
            if (ui.providersError) {
              setText(ui.providersError, formatApiError(err));
              ui.providersError.hidden = false;
            }
          } finally {
            if (ui.providersLoading) {
              ui.providersLoading.hidden = true;
            }
            providersRequest = null;
          }
        })();
        return providersRequest;
      }

      function renderProviders() {
        ui.providersList.replaceChildren();
        ui.providersEmpty.hidden = providers.length !== 0;
        for (const provider of providers) {
          const item = el('article', 'provider-item');
          const main = el('div', 'provider-main');
          const title = el('h3', 'provider-title');
          title.appendChild(el('span', '', provider.label || provider.id));
          if (provider.default) {
            title.appendChild(el('span', 'badge-soft is-default', t('settings.default', '默认')));
          }
          if (!provider.enabled) {
            title.appendChild(el('span', 'badge-soft is-off', t('settings.disabled', '已禁用')));
          }
          main.appendChild(title);

          const meta = el('div', 'provider-meta');
          meta.appendChild(el('span', '', providerTypeLabel(provider.type)));
          meta.appendChild(el('span', '', provider.zone || '—'));
          meta.appendChild(el('span', '', provider.id));
          if (provider.type !== 'manual') {
            const creds = provider.creds || {};
            for (const [key, value] of Object.entries(creds)) {
              if (value) {
                meta.appendChild(el('span', '', key + ': ' + value));
              }
            }
          }
          main.appendChild(meta);
          item.appendChild(main);

          const actions = el('div', 'provider-actions');
          if (provider.type !== 'manual') {
            const test = button(t('settings.test', '测试连接'), 'ghost small');
            test.addEventListener('click', async () => {
              setButtonBusy(test, true, t('settings.testing', '测试中…'));
              try {
                const result = await api('/api/providers/test', { method: 'POST', body: { id: provider.id } });
                if (result.zone_found) {
                  showToastSafe(t('settings.test_ok', '连接成功，凭据有效。'), 'success');
                } else {
                  showToastSafe(result.message || t('settings.test_zone_missing', '连接成功，但找不到配置的主域名。'), 'error');
                }
              } catch (err) {
                if (!handleUnauthorized(err)) {
                  showToastSafe(formatApiError(err), 'error');
                }
              } finally {
                setButtonBusy(test, false);
              }
            });
            actions.appendChild(test);
          }
          const edit = button(t('settings.edit', '编辑'), 'ghost small');
          edit.addEventListener('click', () => openProviderForm(provider));
          actions.appendChild(edit);
          const del = button(t('common.delete', '删除'), 'ghost small danger-text');
          del.addEventListener('click', () => {
            confirmDialog({
              title: t('settings.delete_provider', '删除解析平台'),
              message: tf('settings.delete_provider_confirm', { label: provider.label || provider.id },
                '确认删除解析平台「{label}」？正在被实例使用时无法删除。'),
              onConfirm: async () => {
                await api('/api/providers/' + encodeURIComponent(provider.id), { method: 'DELETE' });
                showToastSafe(t('settings.provider_deleted', '解析平台已删除。'), 'success');
                providers = providers.filter((p) => p.id !== provider.id);
                renderProviders();
              },
            });
          });
          actions.appendChild(del);
          item.appendChild(actions);
          ui.providersList.appendChild(item);
        }
      }

      function credsForType(type, existing) {
        const keys = {
          cloudflare: ['api_token'],
          aliyun: ['access_key_id', 'access_key_secret'],
          dnspod: ['secret_id', 'secret_key'],
          manual: [],
        }[type] || [];
        const wrap = el('div', 'form-grid');
        const inputs = {};
        for (const key of keys) {
          const input = el('input');
          input.type = 'password';
          input.autocomplete = 'off';
          input.spellcheck = false;
          const masked = existing && existing[key] ? existing[key] : '';
          if (masked) {
            input.placeholder = t('settings.creds_keep', '已保存：') + masked + t('settings.creds_keep_hint', '（留空不修改）');
          }
          wrap.appendChild(field(t('setup.' + key, key), input, '', 'full'));
          inputs[key] = input;
        }
        return { node: wrap, inputs, keys };
      }

      function openProviderForm(existing) {
        const body = el('div');
        body.appendChild(el('div', 'alert alert-warning', t('settings.provider_unique',
          '同一平台 + 同一主域名只能有一个配置，保存会覆盖现有凭据。')));

        const form = el('div', 'form-grid');
        const typeSelect = el('select');
        for (const type of ['cloudflare', 'aliyun', 'dnspod', 'manual']) {
          const option = el('option', '', providerTypeLabel(type));
          option.value = type;
          typeSelect.appendChild(option);
        }
        typeSelect.value = existing ? existing.type : 'cloudflare';
        typeSelect.disabled = Boolean(existing);
        form.appendChild(field(t('settings.provider_type', '平台类型'), typeSelect,
          existing ? t('settings.type_locked', '类型创建后不可修改。') : ''));

        const labelInput = textInput(existing ? existing.label : '', t('setup.provider_label', '显示名称'), 80);
        form.appendChild(field(t('setup.provider_label', '显示名称'), labelInput, t('settings.provider_label_hint', '留空则使用平台默认名称。')));

        const zoneInput = textInput(existing ? existing.zone : '', 'example.com', 253);
        form.appendChild(field(t('setup.provider_zone', '主域名'), zoneInput,
          t('setup.provider_zone_desc', '与 DNS 托管平台上的 Zone 一致。')));

        const credsWrap = el('div', 'field full');
        form.appendChild(credsWrap);
        let credsFields = null;
        let testButton = null;
        function renderCreds() {
          const result = credsForType(typeSelect.value, existing ? existing.creds : null);
          credsFields = result;
          credsWrap.replaceChildren(result.node);
          if (result.keys.length === 0) {
            credsWrap.appendChild(el('p', 'hint', t('setup.manual_hint', '手动解析模式无需凭据。')));
          }
          if (testButton) {
            testButton.hidden = typeSelect.value === 'manual';
          }
        }
        renderCreds();
        typeSelect.addEventListener('change', renderCreds);
        body.appendChild(form);

        const testResult = el('div', 'alert');
        testResult.hidden = true;
        body.appendChild(testResult);

        const error = el('div', 'alert alert-error');
        error.hidden = true;
        body.appendChild(error);

        function collectCreds() {
          const creds = {};
          if (!credsFields) {
            return creds;
          }
          for (const key of credsFields.keys) {
            const value = credsFields.inputs[key].value.trim();
            if (value) {
              creds[key] = value;
            }
          }
          return creds;
        }

        const cancel = button(t('common.cancel', '取消'), 'ghost');
        cancel.addEventListener('click', closeModal);
        const test = button(t('settings.test', '测试连接'), 'ghost');
        testButton = test;
        test.hidden = typeSelect.value === 'manual';
        test.addEventListener('click', async () => {
          const type = typeSelect.value;
          const zone = zoneInput.value.trim().toLowerCase();
          if (!zone) {
            testResult.hidden = false;
            testResult.className = 'alert alert-error';
            setText(testResult, t('setup.provider_error_zone', '请填写有效的主域名，例如 example.com。'));
            return;
          }
          setButtonBusy(test, true, t('settings.testing', '测试中…'));
          testResult.hidden = true;
          try {
            const payload = { type, zone, creds: collectCreds() };
            if (existing) {
              payload.id = existing.id;
            }
            const result = await api('/api/providers/test', { method: 'POST', body: payload });
            testResult.hidden = false;
            testResult.className = result.zone_found ? 'alert alert-success' : 'alert alert-warning';
            const zones = Array.isArray(result.zones) ? result.zones.map((z) => z.name).filter(Boolean).join('、') : '';
            setText(testResult, (result.message || '') + (zones ? ' ' + tf('settings.zone_list', { zones }, '可见主域名：{zones}') : ''));
          } catch (err) {
            testResult.hidden = false;
            testResult.className = 'alert alert-error';
            setText(testResult, formatApiError(err));
          } finally {
            setButtonBusy(test, false);
          }
        });
        const save = button(t('common.save', '保存'), 'primary');
        save.addEventListener('click', async () => {
          error.hidden = true;
          const zone = zoneInput.value.trim().toLowerCase();
          if (!zone) {
            error.hidden = false;
            setText(error, t('setup.provider_error_zone', '请填写有效的主域名，例如 example.com。'));
            return;
          }
          const creds = collectCreds();
          if (!existing && typeSelect.value !== 'manual' && Object.keys(creds).length === 0) {
            error.hidden = false;
            setText(error, t('setup.provider_error_creds', '请填写所选平台需要的凭据。'));
            return;
          }
          setButtonBusy(save, true, t('common.saving', '保存中…'));
          try {
            const payload = {
              type: typeSelect.value,
              label: labelInput.value.trim(),
              zone,
              creds,
            };
            if (existing) {
              await api('/api/providers/' + encodeURIComponent(existing.id), { method: 'PUT', body: payload });
            } else {
              await api('/api/providers', { method: 'POST', body: payload });
            }
            showToastSafe(t('settings.provider_saved', '解析平台已保存。'), 'success');
            closeModal();
            providers = [];
            await loadProviders(true);
          } catch (err) {
            if (!handleUnauthorized(err)) {
              error.hidden = false;
              setText(error, formatApiError(err));
            }
          } finally {
            setButtonBusy(save, false);
          }
        });

        body.appendChild(modalActions([cancel, test, save]));
        openModal(existing ? t('settings.edit_provider', '编辑解析平台') : t('settings.add_provider', '添加解析平台'), body);
      }

      // --------------------------------------------------------------- settings

      async function loadPublicIP() {
        try {
          const settings = await api('/api/settings');
          publicIP = settings && settings.public_ip ? String(settings.public_ip) : '';
        } catch (err) {
          publicIP = '';
        }
      }

      async function loadPublicIPSettings() {
        ui.settingsDetectLine.textContent = '';
        ui.useDetectedIP.hidden = true;
        ui.settingsIPAlert.hidden = true;
        try {
          const settings = await api('/api/settings?detect=1');
          publicIP = settings.public_ip ? String(settings.public_ip) : '';
          ui.settingsPublicIP.value = publicIP;
          ui.settingsExternalCheck.checked = Boolean(settings.external_check);
          detectedIP = settings.detected_ip || '';
          if (detectedIP) {
            setText(ui.settingsDetectLine, tf('settings.detected', { ip: detectedIP }, '当前探测到公网 IP：{ip}'));
            ui.useDetectedIP.hidden = false;
          } else if (settings.detect_error) {
            setText(ui.settingsDetectLine, settings.detect_error);
            ui.useDetectedIP.hidden = true;
          } else {
            setText(ui.settingsDetectLine, t('settings.not_detected', '未探测到公网 IP，可手动填写。'));
            ui.useDetectedIP.hidden = true;
          }
        } catch (err) {
          if (handleUnauthorized(err)) {
            return;
          }
          setText(ui.settingsDetectLine, t('settings.detect_failed', '自动探测失败，请手动填写公网 IP。'));
          ui.useDetectedIP.hidden = true;
        }
      }

      async function savePublicIP() {
        const value = ui.settingsPublicIP.value.trim();
        if (value && !isIPv4(value)) {
          ui.settingsIPAlert.hidden = false;
          ui.settingsIPAlert.className = 'alert alert-error';
          setText(ui.settingsIPAlert, t('setup.ip_error', '公网 IP 格式不正确（IPv4）。'));
          ui.settingsPublicIP.focus();
          return;
        }
        setButtonBusy(ui.savePublicIP, true, t('common.saving', '保存中…'));
        try {
          const result = await api('/api/settings', { method: 'PUT', body: { public_ip: value } });
          publicIP = result && result.public_ip ? String(result.public_ip) : '';
          ui.settingsIPAlert.hidden = false;
          ui.settingsIPAlert.className = 'alert alert-success';
          setText(ui.settingsIPAlert, value
            ? t('settings.ip_saved', '公网 IP 已保存。')
            : t('settings.ip_cleared', '已清除保存的公网 IP，下次绑定将尝试自动探测。'));
          if (result && result.public_ip) {
            ui.settingsPublicIP.value = result.public_ip;
          }
        } catch (err) {
          if (handleUnauthorized(err)) {
            return;
          }
          ui.settingsIPAlert.hidden = false;
          ui.settingsIPAlert.className = 'alert alert-error';
          setText(ui.settingsIPAlert, formatApiError(err));
        } finally {
          setButtonBusy(ui.savePublicIP, false);
        }
      }

      // --------------------------------------------------------------- backups

      function setBackupAlert(node, kind, message) {
        if (!node) {
          return;
        }
        if (!message) {
          node.hidden = true;
          node.textContent = '';
          return;
        }
        node.hidden = false;
        node.className = 'alert ' + (kind === 'error' ? 'alert-error' : kind === 'warning' ? 'alert-warning' : 'alert-success');
        setText(node, message);
      }

      function backupSubtitle(rec, options) {
        const opts = options || {};
        const parts = [formatDate(rec.created_at), formatBytes(rec.size_bytes), rec.auto ? t('backup.auto', '自动') : t('backup.manual', '手动')];
        if (opts.showInstance) {
          parts.unshift((rec.name || rec.instance_id) + ' (' + rec.instance_id + ')');
        }
        if (opts.orphan) {
          parts.push(t('backup.orphan', '实例已删除，恢复会按原端口重建'));
        }
        return parts.join(' · ');
      }

      function renderBackupList(container, items, options) {
        const opts = options || {};
        container.replaceChildren();
        for (const rec of items) {
          const orphan = opts.orphan === true || rec.instance_exists === false;
          const row = el('div', 'backup-item' + (orphan ? ' is-orphan' : ''));
          const info = el('div', 'backup-info');
          info.appendChild(el('span', 'backup-file', rec.file));
          info.appendChild(el('span', 'backup-sub', backupSubtitle(rec, { showInstance: opts.showInstance, orphan })));
          row.appendChild(info);

          const actions = el('div', 'backup-actions');
          if (rec.exists === false) {
            actions.appendChild(el('span', 'hint', t('backup.file_missing', '文件已丢失')));
          } else {
            const download = button(t('backup.download', '下载'), 'ghost small');
            download.addEventListener('click', () => {
              downloadBackup(rec.instance_id, rec.file);
            });
            actions.appendChild(download);

            const restore = button(t('backup.restore', '恢复'), 'small');
            restore.addEventListener('click', () => {
              confirmRestore(rec, { orphan });
            });
            actions.appendChild(restore);
          }
          row.appendChild(actions);
          container.appendChild(row);
        }
      }

      function downloadBackup(instanceID, file) {
        const link = document.createElement('a');
        link.href = '/api/instances/' + encodeURIComponent(instanceID) + '/backup?file=' + encodeURIComponent(file);
        link.download = file;
        document.body.appendChild(link);
        link.click();
        link.remove();
      }

      function confirmRestore(rec, options) {
        const opts = options || {};
        const message = opts.orphan
          ? tf('backup.restore_orphan_confirm', { file: rec.file, id: rec.instance_id },
            '将用备份「{file}」重建实例 {id}（沿用原端口与域名）。恢复期间会覆盖同名数据并启动容器。')
          : tf('backup.restore_confirm', { file: rec.file },
            '将用备份「{file}」覆盖当前实例的数据目录。恢复前会停止容器，失败会自动回滚；成功后自动启动并校验运行状态。');
        confirmDialog({
          title: t('backup.restore_title', '恢复备份'),
          message,
          confirmLabel: t('backup.restore', '恢复'),
          danger: true,
          onConfirm: async () => {
            await api('/api/instances/' + encodeURIComponent(rec.instance_id) + '/restore', {
              method: 'POST',
              body: { file: rec.file },
            });
            showToastSafe(t('backup.restored', '恢复完成，容器已启动。'), 'success');
            if (opts.orphan) {
              navigate('#/instances/' + encodeURIComponent(rec.instance_id));
              return;
            }
            if (currentView === 'settings') {
              await loadAllBackups();
            } else if (currentDetail && currentDetail.id === rec.instance_id) {
              await loadDetail(rec.instance_id, false);
              await loadBackups();
            }
          },
        });
      }

      async function loadBackups() {
        if (!currentDetail) {
          return;
        }
        const id = currentDetail.id;
        ui.backupsLoading.hidden = false;
        ui.backupsError.hidden = true;
        ui.backupsEmpty.hidden = true;
        try {
          const items = await api('/api/instances/' + encodeURIComponent(id) + '/backups');
          renderBackupList(ui.backupsList, Array.isArray(items) ? items : [], {});
          setText(ui.backupMeta, tf('backup.count', { count: Array.isArray(items) ? items.length : 0 }, '共 {count} 份备份'));
          ui.backupsEmpty.hidden = Array.isArray(items) && items.length > 0;
        } catch (err) {
          if (!handleUnauthorized(err)) {
            ui.backupsError.hidden = false;
            setText(ui.backupsError, formatApiError(err));
          }
        } finally {
          ui.backupsLoading.hidden = true;
        }
      }

      async function createBackup(buttonNode) {
        if (!currentDetail) {
          return;
        }
        const id = currentDetail.id;
        setButtonBusy(buttonNode, true, t('backup.creating', '备份中…'));
        setBackupAlert(ui.backupAlert, '', '');
        try {
          const rec = await api('/api/instances/' + encodeURIComponent(id) + '/backup', { method: 'POST' });
          setBackupAlert(ui.backupAlert, 'success', tf('backup.created', { file: rec.file }, '备份已创建：{file}'));
          await loadBackups();
          await loadDetail(id, false);
        } catch (err) {
          if (!handleUnauthorized(err)) {
            setBackupAlert(ui.backupAlert, 'error', formatApiError(err));
          }
        } finally {
          setButtonBusy(buttonNode, false);
        }
      }

      async function uploadBackupFile(file) {
        if (!currentDetail || !file) {
          return;
        }
        const id = currentDetail.id;
        confirmDialog({
          title: t('backup.upload_title', '上传并恢复'),
          message: tf('backup.upload_confirm', { file: file.name },
            '将上传「{file}」并覆盖实例数据目录。上传的包会先保存为一条备份记录，再执行恢复。'),
          confirmLabel: t('backup.upload_restore', '上传并恢复'),
          danger: true,
          onConfirm: async () => {
            const form = new FormData();
            form.append('file', file);
            await api('/api/instances/' + encodeURIComponent(id) + '/restore', { method: 'POST', form });
            showToastSafe(t('backup.restored', '恢复完成，容器已启动。'), 'success');
            await loadDetail(id, false);
            await loadBackups();
          },
        });
      }

      function applyBackupPlan(plan) {
        backupPlan = plan;
        ui.backupEnabled.checked = Boolean(plan.enabled);
        ui.backupMode.value = plan.mode === 'interval' ? 'interval' : 'daily';
        ui.backupDailyAt.value = plan.daily_at || '04:30';
        ui.backupIntervalHours.value = plan.interval_hours || 24;
        ui.backupRetention.value = plan.retention || 7;
        ui.backupPlanState.textContent = plan.enabled ? t('backup.plan_on', '已启用') : t('backup.plan_off', '已关闭');
        ui.backupPlanState.className = 'status-badge ' + (plan.enabled ? 'is-running' : 'is-stopped');

        const parts = [];
        parts.push(plan.last_success_at
          ? tf('backup.last_success', { time: formatDate(plan.last_success_at) }, '上次成功：{time}')
          : t('backup.last_success_none', '上次成功：暂无'));
        parts.push(plan.next_run_at
          ? tf('backup.next_run', { time: formatDate(plan.next_run_at) }, '下次执行：{time}')
          : t('backup.next_run_none', '下次执行：—'));
        if (plan.last_error) {
          parts.push(tf('backup.last_error', { message: plan.last_error }, '上次错误：{message}'));
        }
        setText(ui.backupPlanStatus, parts.join(' · '));
        syncBackupModeFields();
      }

      function syncBackupModeFields() {
        const daily = ui.backupMode.value !== 'interval';
        ui.backupDailyAt.disabled = !daily;
        ui.backupIntervalHours.disabled = daily;
      }

      async function loadBackupPlan() {
        try {
          const plan = await api('/api/backup/plan');
          applyBackupPlan(plan);
        } catch (err) {
          if (!handleUnauthorized(err)) {
            setBackupAlert(ui.backupPlanAlert, 'error', formatApiError(err));
          }
        }
      }

      async function saveBackupPlan() {
        const interval = Number.parseInt(ui.backupIntervalHours.value, 10);
        const retention = Number.parseInt(ui.backupRetention.value, 10);
        if (!Number.isFinite(interval) || interval < 1 || interval > 720) {
          setBackupAlert(ui.backupPlanAlert, 'error', t('backup.interval_invalid', '间隔小时数需要在 1–720 之间。'));
          return;
        }
        if (!Number.isFinite(retention) || retention < 1 || retention > 100) {
          setBackupAlert(ui.backupPlanAlert, 'error', t('backup.retention_invalid', '保留份数需要在 1–100 之间。'));
          return;
        }
        setButtonBusy(ui.saveBackupPlan, true, t('common.saving', '保存中…'));
        try {
          const plan = await api('/api/backup/plan', {
            method: 'PUT',
            body: {
              enabled: ui.backupEnabled.checked,
              mode: ui.backupMode.value,
              daily_at: ui.backupDailyAt.value || '04:30',
              interval_hours: interval,
              retention,
            },
          });
          applyBackupPlan(plan);
          setBackupAlert(ui.backupPlanAlert, 'success', t('backup.plan_saved', '备份计划已保存。'));
        } catch (err) {
          if (!handleUnauthorized(err)) {
            setBackupAlert(ui.backupPlanAlert, 'error', formatApiError(err));
          }
        } finally {
          setButtonBusy(ui.saveBackupPlan, false);
        }
      }

      async function loadAllBackups() {
        ui.allBackupsLoading.hidden = false;
        setBackupAlert(ui.allBackupsAlert, '', '');
        try {
          const items = await api('/api/backups');
          const list = Array.isArray(items) ? items : [];
          renderBackupList(ui.allBackupsList, list, { showInstance: true });
          if (list.length === 0) {
            ui.allBackupsList.appendChild(el('p', 'muted', t('backup.empty', '还没有备份。')));
          }
        } catch (err) {
          if (!handleUnauthorized(err)) {
            setBackupAlert(ui.allBackupsAlert, 'error', formatApiError(err));
          }
        } finally {
          ui.allBackupsLoading.hidden = true;
        }
      }

      async function saveExternalCheck() {
        setButtonBusy(ui.saveExternalCheck, true, t('common.saving', '保存中…'));
        try {
          const result = await api('/api/settings', {
            method: 'PUT',
            body: { external_check: ui.settingsExternalCheck.checked },
          });
          ui.settingsExternalCheck.checked = Boolean(result.external_check);
          setBackupAlert(ui.externalCheckAlert, 'success', result.external_check
            ? t('settings.external_check_on', '已启用第三方探测（check-host.net）。')
            : t('settings.external_check_off', '已关闭第三方探测。'));
        } catch (err) {
          if (!handleUnauthorized(err)) {
            setBackupAlert(ui.externalCheckAlert, 'error', formatApiError(err));
          }
        } finally {
          setButtonBusy(ui.saveExternalCheck, false);
        }
      }

      // ------------------------------------------------------------------ delete

      function openDeleteModal(detail) {
        const body = el('div');
        body.appendChild(el('p', '', tf('detail.delete_warning', { name: detail.name || detail.id },
          '确认删除实例「{name}」？此操作不可撤销。')));

        const purgeRow = el('label', 'option-row');
        const purge = el('input');
        purge.type = 'checkbox';
        purge.checked = true;
        purge.setAttribute('aria-label', t('detail.delete_purge', '同时删除数据目录（身份密钥、SQLite、files）'));
        purgeRow.appendChild(purge);
        purgeRow.appendChild(el('span', '', t('detail.delete_purge', '同时删除数据目录（身份密钥、SQLite、files）')));
        body.appendChild(purgeRow);

        const dnsRow = el('label', 'option-row');
        const dns = el('input');
        dns.type = 'checkbox';
        dns.checked = true;
        dns.setAttribute('aria-label', t('detail.delete_dns', '同时删除面板创建的 DNS 记录'));
        dnsRow.appendChild(dns);
        dnsRow.appendChild(el('span', '', t('detail.delete_dns', '同时删除面板创建的 DNS 记录')));
        body.appendChild(dnsRow);

        const note = el('div', 'alert alert-warning');
        note.textContent = t('detail.delete_dns_note',
          '面板只会删除自己创建的记录；手动添加的记录需要你自己去 DNS 控制台删除。');
        body.appendChild(note);

        const backupNote = el('p', 'hint');
        setText(backupNote, t('detail.delete_backup_note',
          '已有备份不会被删除；删除后仍可在设置页的「全部实例备份」里恢复，并按原端口与地址重建实例。'));
        body.appendChild(backupNote);

        const cancel = button(t('common.cancel', '取消'), 'ghost');
        cancel.addEventListener('click', closeModal);
        const confirm = button(t('detail.delete_confirm', '确认删除'), 'danger');
        confirm.addEventListener('click', async () => {
          setButtonBusy(confirm, true, t('common.loading', '处理中…'));
          cancel.disabled = true;
          try {
            const query = '?purge=' + (purge.checked ? 'true' : 'false') + '&dns=' + (dns.checked ? 'true' : 'false');
            await api('/api/instances/' + encodeURIComponent(detail.id) + query, { method: 'DELETE' });
            closeModal();
            showToastSafe(t('detail.deleted', '实例已删除。'), 'success');
            navigate('#/');
            await loadInstances(false);
          } catch (err) {
            setButtonBusy(confirm, false);
            cancel.disabled = false;
            if (!handleUnauthorized(err)) {
              showToastSafe(formatApiError(err), 'error');
            }
          }
        });
        body.appendChild(modalActions([cancel, confirm]));
        openModal(t('detail.delete_title', '删除实例'), body);
      }

      // -------------------------------------------------------------------- auth

      function showAuthAlert(message, kind) {
        ui.authError.textContent = message;
        ui.authError.className = kind === 'success' ? 'alert alert-success' : 'alert alert-error';
        ui.authError.hidden = false;
      }

      function clearAuthAlert() {
        ui.authError.hidden = true;
        ui.authError.textContent = '';
      }

      function showAuthPanel(name, clear) {
        const showLogin = name === 'login';
        ui.loginPanel.hidden = !showLogin;
        ui.forgotPanel.hidden = showLogin;
        if (clear) {
          clearAuthAlert();
        }
        if (!showLogin) {
          ui.forgotUsername.value = ui.loginUsername.value.trim();
          ui.forgotCode.value = '';
          ui.forgotPassword.value = '';
          ui.forgotConfirm.value = '';
        }
      }

      function showAuth() {
        authenticated = false;
        stopListPolling();
        stopDetailPolling();
        closeModal();
        ui.loading.hidden = true;
        ui.authView.hidden = false;
        ui.dashboardView.hidden = true;
        ui.topbarUser.hidden = true;
        ui.currentUser.textContent = '';
      }

      async function showDashboard(username) {
        authenticated = true;
        ui.loading.hidden = true;
        ui.authView.hidden = true;
        ui.dashboardView.hidden = false;
        ui.topbarUser.hidden = false;
        ui.currentUser.textContent = username ? String(username) : '';
        clearAuthAlert();
        if (!routeBound) {
          routeBound = true;
          window.addEventListener('hashchange', () => {
            void route();
          });
        }
        await Promise.all([route(), loadPublicIP(), loadProviders(false).catch(() => {})]);
      }

      // ------------------------------------------------------------------- wiring

      ui.navInstances.addEventListener('click', () => {
        navigate('#/');
      });
      ui.navPorts.addEventListener('click', () => {
        navigate('#/ports');
      });
      ui.navSettings.addEventListener('click', () => {
        navigate('#/settings');
      });
      ui.detailBack.addEventListener('click', () => {
        navigate('#/');
      });
      ui.detailErrorBack.addEventListener('click', () => {
        navigate('#/');
      });
      ui.detailRetry.addEventListener('click', () => {
        if (currentDetailID) {
          void loadDetail(currentDetailID, true);
        }
      });
      ui.newInstance.addEventListener('click', () => {
        void openCreateWizard();
      });
      ui.addProvider.addEventListener('click', () => {
        openProviderForm(null);
      });

      ui.refreshPorts.addEventListener('click', () => {
        void loadPorts(false);
      });
      ui.copyPortChecklist.addEventListener('click', async () => {
        if (!portsData || !portsData.text) {
          return;
        }
        try {
          await copyText(portsData.text);
          showToastSafe(t('ports.copied', '端口清单已复制。'), 'success');
        } catch (err) {
          showToastSafe(t('common.copy_failed', '复制失败，请手动选择。'), 'error');
        }
      });
      ui.checkPorts.addEventListener('click', () => {
        void runPortsCheck();
      });

      ui.tokenToggle.addEventListener('click', () => {
        tokenVisible = !tokenVisible;
        renderToken();
      });
      ui.tokenCopy.addEventListener('click', async () => {
        if (!currentDetail || !currentDetail.admin_token) {
          return;
        }
        try {
          await copyText(currentDetail.admin_token);
          showToastSafe(t('common.copied', '已复制'), 'success');
        } catch (err) {
          showToastSafe(t('common.copy_failed', '复制失败，请手动选择。'), 'error');
        }
      });
      ui.copyPorts.addEventListener('click', async () => {
        if (!currentDetail) {
          return;
        }
        const ports = currentDetail.ports || {};
        const text = 'UDP ' + portValue(ports.voice) + ' (语音)\nTCP ' + portValue(ports.filetransfer) + ' (文件传输)';
        try {
          await copyText(text);
          showToastSafe(t('common.copied', '已复制'), 'success');
        } catch (err) {
          showToastSafe(t('common.copy_failed', '复制失败，请手动选择。'), 'error');
        }
      });
      ui.refreshDetail.addEventListener('click', () => {
        if (currentDetailID) {
          void loadDetail(currentDetailID, false);
        }
      });

      ui.detailStart.addEventListener('click', () => {
        void runLifecycle('start', ui.detailStart);
      });
      ui.detailStop.addEventListener('click', () => {
        void runLifecycle('stop', ui.detailStop);
      });
      ui.detailRestart.addEventListener('click', () => {
        void runLifecycle('restart', ui.detailRestart);
      });
      ui.deleteInstance.addEventListener('click', () => {
        if (currentDetail) {
          openDeleteModal(currentDetail);
        }
      });

      ui.bindDomain.addEventListener('click', () => {
        if (currentDetail) {
          openBindModal(currentDetail);
        }
      });
      ui.verifyDomain.addEventListener('click', async () => {
        if (!currentDetail) {
          return;
        }
        verifyNotice = null;
        const result = await verifyInstanceDomain(currentDetail.id, ui.verifyDomain, null);
        if (result) {
          verifyNotice = {
            ok: Boolean(result.verified),
            message: result.verified
              ? t('detail.verify_ok', '解析已验证。')
              : (result.message || t('detail.verify_pending', '记录仍在传播，请稍后再试。')),
          };
          await loadDetail(currentDetail.id, false);
        }
      });
      ui.manualDomain.addEventListener('click', () => {
        if (currentDetail) {
          void openManualChecklist(currentDetail.id);
        }
      });
      ui.unbindDomain.addEventListener('click', () => {
        if (!currentDetail) {
          return;
        }
        const id = currentDetail.id;
        confirmDialog({
          title: t('detail.unbind_title', '解绑域名'),
          message: t('detail.unbind_confirm', '将删除面板创建的 A / SRV 记录，实例本身保留并可按 IP:端口 连接。手动添加的记录需要你自己删除。'),
          confirmLabel: t('detail.unbind', '解绑域名'),
          onConfirm: async () => {
            await api('/api/instances/' + encodeURIComponent(id) + '/domain', { method: 'DELETE' });
            showToastSafe(t('detail.unbound', '域名已解绑。'), 'success');
            await loadDetail(id, false);
          },
        });
      });

      ui.useDetectedIP.addEventListener('click', () => {
        if (detectedIP) {
          ui.settingsPublicIP.value = detectedIP;
        }
      });
      ui.savePublicIP.addEventListener('click', () => {
        void savePublicIP();
      });

      ui.createBackup.addEventListener('click', () => {
        void createBackup(ui.createBackup);
      });
      ui.refreshBackups.addEventListener('click', () => {
        void loadBackups();
      });
      ui.uploadBackup.addEventListener('change', () => {
        const file = ui.uploadBackup.files && ui.uploadBackup.files[0];
        ui.uploadBackup.value = '';
        if (file) {
          void uploadBackupFile(file);
        }
      });
      ui.refreshAllBackups.addEventListener('click', () => {
        void loadAllBackups();
      });
      ui.saveBackupPlan.addEventListener('click', () => {
        void saveBackupPlan();
      });
      ui.refreshBackupPlan.addEventListener('click', () => {
        void loadBackupPlan();
      });
      ui.backupMode.addEventListener('change', syncBackupModeFields);
      ui.saveExternalCheck.addEventListener('click', () => {
        void saveExternalCheck();
      });

      ui.modalClose.addEventListener('click', closeModal);
      ui.modalRoot.addEventListener('click', (event) => {
        if (event.target === ui.modalRoot) {
          closeModal();
        }
      });
      document.addEventListener('keydown', (event) => {
        if (event.key === 'Escape' && !ui.modalRoot.hidden) {
          closeModal();
        }
      });

      ui.loginForm.addEventListener('submit', async (event) => {
        event.preventDefault();
        const username = ui.loginUsername.value.trim();
        const password = ui.loginPassword.value;
        if (!username || !password) {
          showAuthAlert(t('login.error_required'));
          return;
        }
        setButtonBusy(ui.loginSubmit, true, t('common.loading'));
        try {
          const result = await api('/api/login', {
            method: 'POST',
            body: { username, password },
          });
          ui.loginPassword.value = '';
          await showDashboard(result.username || username);
        } catch (err) {
          showAuthAlert(formatApiError(err));
        } finally {
          setButtonBusy(ui.loginSubmit, false);
        }
      });

      ui.forgotForm.addEventListener('submit', async (event) => {
        event.preventDefault();
        const username = ui.forgotUsername.value.trim();
        const recoveryCode = ui.forgotCode.value.trim();
        const newPassword = ui.forgotPassword.value;
        if (!username || !recoveryCode || !newPassword) {
          showAuthAlert(t('login.error_reset_required'));
          return;
        }
        const problem = passwordProblem(newPassword);
        if (problem) {
          showAuthAlert(problem);
          return;
        }
        if (newPassword !== ui.forgotConfirm.value) {
          showAuthAlert(t('login.error_password_mismatch'));
          return;
        }

        setButtonBusy(ui.forgotSubmit, true, t('common.loading'));
        try {
          const result = await api('/api/password/forgot', {
            method: 'POST',
            body: {
              username,
              recovery_code: recoveryCode,
              new_password: newPassword,
            },
          });
          ui.loginUsername.value = username;
          ui.loginPassword.value = '';
          ui.forgotPassword.value = '';
          ui.forgotConfirm.value = '';
          showAuthPanel('login', false);
          const message = result && typeof result.message === 'string'
            ? result.message
            : tf('login.reset_success', { remaining: 0 });
          showAuthAlert(message, 'success');
        } catch (err) {
          showAuthAlert(formatApiError(err));
        } finally {
          setButtonBusy(ui.forgotSubmit, false);
        }
      });

      ui.showForgot.addEventListener('click', () => {
        showAuthPanel('forgot', true);
      });
      ui.forgotBack.addEventListener('click', () => {
        showAuthPanel('login', true);
      });
      ui.logout.addEventListener('click', async () => {
        try {
          await api('/api/logout', { method: 'POST' });
        } catch (err) {
          // The local UI returns to login regardless; the session is cleared
          // server-side when the request succeeds.
        }
        showAuth();
        showAuthPanel('login', true);
        showAuthAlert(t('login.logged_out'), 'success');
      });

      try {
        const session = await api('/api/session');
        if (session.authenticated) {
          await showDashboard(session.username);
        } else {
          showAuth();
          showAuthPanel('login', false);
        }
      } catch (err) {
        showAuth();
        showAuthPanel('login', false);
        showAuthAlert(formatApiError(err));
      } finally {
        ui.loading.hidden = true;
      }
    }

  async function boot() {
    await loadI18n();
    applyI18n();
    const page = document.body.dataset.page;
    document.title = page === 'setup'
      ? t('setup.title') + ' · ' + t('app.name')
      : t('app.name');
    if (page === 'setup') {
      await initSetup();
    } else {
      await initIndex();
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => {
      void boot();
    }, { once: true });
  } else {
    void boot();
  }
})();
