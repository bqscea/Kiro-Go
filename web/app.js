/*
 * Kiro-Go admin UI logic.
 */
(() => {
  'use strict';

  // State
  const baseUrl = location.origin;
  if (localStorage.getItem('kiro_remember') !== '1') {
    localStorage.removeItem('admin_password');
    localStorage.removeItem('admin_login_time');
  }
  let password = sessionStorage.getItem('admin_password') || localStorage.getItem('admin_password') || '';
  let currentLang = localStorage.getItem('kiro_lang') || 'zh';
  const dict = { en: null, zh: null };
  let accountsData = [];
  const selectedAccounts = new Set();
  let filterKeyword = '';
  let filterStatus = 'all';
  let filterGroup = 'all';
  let sortField = '';
  let privacyModeEnabled = true;
  let promptRules = [];
  let builderIdSession = '';
  let builderIdPollTimer = null;
  let iamSession = '';
  let exportSelectedIds = new Set();
  let currentVersion = '';
  let testLogs = [];
  let testModalAccountId = '';
  let testModalModels = [];
  let testModalLoadingModels = false;
  let testModalModelError = false;
  let testModalRunning = false;
  let customSelectUid = 0;
  let customSelectObserver = null;
  let customSelectRefreshQueued = false;
  let apiKeysData = [];
  let groupPoliciesData = [];
  let modelAliasesData = [];
  let observeTimer = null;
  let observeSSE = null;
  let observeActive = false;
  let requestsActive = false;
  let statsActive = false;
  let statsTimer = null;
  let allGroups = [];
  let currentGroupAccountId = null;
  let currentBatchGroupIds = null;
  const originalTitle = document.title || 'Kiro-Go';
  let alertFlashTimer = null;
  let alertCount = 0;

  // DOM helpers
  const $ = (id) => document.getElementById(id);
  const qsa = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  function escapeHtml(s) {
    const d = document.createElement('div');
    d.textContent = s == null ? '' : String(s);
    return d.innerHTML;
  }
  function escapeAttr(s) {
    return escapeHtml(s).replace(/"/g, '&quot;');
  }
  async function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try {
        await navigator.clipboard.writeText(text);
        return;
      } catch (e) { }
    }
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.className = 'clipboard-proxy';
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    document.body.removeChild(ta);
  }
  function renderEndpointCode(id, value) {
    const el = $(id);
    if (!el) return;
    const raw = String(value || '');
    el.dataset.rawValue = raw;
    try {
      const url = new URL(raw);
      const path = url.pathname + url.search + url.hash;
      el.innerHTML =
        '<span class="api-code-protocol">' + escapeHtml(url.protocol + '//') + '</span>' +
        '<span class="api-code-host">' + escapeHtml(url.host) + '</span>' +
        '<span class="api-code-path">' + escapeHtml(path) + '</span>';
    } catch (e) {
      el.textContent = raw;
    }
  }

  // i18n
  async function loadLocale(lang) {
    if (dict[lang]) return dict[lang];
    try {
      const res = await fetch('/admin/locales/' + lang + '.json?v=' + Date.now(), { cache: 'no-store' });
      dict[lang] = await res.json();
    } catch (e) {
      dict[lang] = {};
    }
    return dict[lang];
  }
  function t(key, ...args) {
    const active = dict[currentLang] || {};
    const fallback = dict.zh || {};
    let text = active[key] || fallback[key] || key;
    args.forEach((arg, idx) => { text = text.replace('{' + idx + '}', arg); });
    return text;
  }
  function applyTranslations() {
    qsa('[data-i18n]').forEach(el => { el.textContent = t(el.dataset.i18n); });
    qsa('[data-i18n-placeholder]').forEach(el => { el.placeholder = t(el.dataset.i18nPlaceholder); });
    qsa('[data-i18n-title]').forEach(el => { el.title = t(el.dataset.i18nTitle); });
    qsa('[data-i18n-aria-label]').forEach(el => { el.setAttribute('aria-label', t(el.dataset.i18nAriaLabel)); });
    document.title = t('app.title');
    document.documentElement.lang = currentLang;
    updateLangButtons();
    applyTheme(getThemePref());
    refreshCustomSelects();
  }
  async function setLang(lang) {
    currentLang = lang;
    localStorage.setItem('kiro_lang', lang);
    await loadLocale(lang);
    applyTranslations();
    renderVersionBadge();
    renderAccounts();
    renderPromptRules();
    renderApiKeyTable();
    renderGroupPolicies();
    renderModelAliases();
  }
  function updateLangButtons() {
    qsa('.lang-btn').forEach(btn => btn.classList.toggle('active', btn.dataset.lang === currentLang));
    qsa('.lang-toggle').forEach(btn => {
      const label = btn.querySelector('.lang-toggle-label');
      if (label) label.textContent = currentLang === 'zh' ? t('lang.zh') : t('lang.en');
    });
  }
  function toggleLang() {
    setLang(currentLang === 'zh' ? 'en' : 'zh');
  }

  // Custom select
  function getCustomSelectLabel(select) {
    const option = select.selectedOptions && select.selectedOptions[0];
    return ((option && option.textContent) || select.value || '').trim();
  }
  function syncCustomSelect(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const value = wrap.querySelector('.custom-select-value');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (value) value.textContent = getCustomSelectLabel(select);
    if (trigger) trigger.disabled = select.disabled;
    wrap.classList.toggle('is-disabled', select.disabled);
    qsa('.custom-select-option', wrap).forEach(option => {
      const selected = option.dataset.index === String(select.selectedIndex);
      option.classList.toggle('is-selected', selected);
      option.setAttribute('aria-selected', String(selected));
    });
  }
  function renderCustomSelectOptions(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const content = wrap.querySelector('.custom-select-content');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (!content) return;
    if (trigger) labelCustomSelect(select, trigger, content, select.id);
    content.innerHTML = '';
    Array.from(select.options).forEach((option, index) => {
      const item = document.createElement('button');
      item.type = 'button';
      item.className = 'custom-select-option';
      item.setAttribute('role', 'option');
      item.dataset.index = String(index);
      item.disabled = option.disabled;
      item.textContent = (option.textContent || option.value || '').trim();
      content.appendChild(item);
    });
    syncCustomSelect(select);
  }
  function placeCustomSelectContent(select) {
    const wrap = select && select.__customSelect;
    if (!wrap || !wrap.classList.contains('is-open')) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.querySelector('.custom-select-content');
    if (!trigger || !content) return;
    const rect = trigger.getBoundingClientRect();
    const gap = 4;
    const below = window.innerHeight - rect.bottom - gap;
    const above = rect.top - gap;
    const openUp = below < 180 && above > below;
    const available = Math.max(96, Math.min(224, (openUp ? above : below) - 4));
    content.style.left = Math.round(rect.left) + 'px';
    content.style.width = Math.round(rect.width) + 'px';
    content.style.maxHeight = Math.round(available) + 'px';
    content.style.top = openUp ? 'auto' : Math.round(rect.bottom + gap) + 'px';
    content.style.bottom = openUp ? Math.round(window.innerHeight - rect.top + gap) + 'px' : 'auto';
    content.dataset.side = openUp ? 'top' : 'bottom';
  }
  function setCustomSelectOpen(select, open) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.querySelector('.custom-select-content');
    if (!trigger || !content) return;
    if (open && !select.disabled) {
      closeAllCustomSelects(select);
      renderCustomSelectOptions(select);
      wrap.classList.add('is-open');
      trigger.setAttribute('aria-expanded', 'true');
      content.hidden = false;
      placeCustomSelectContent(select);
      requestAnimationFrame(() => placeCustomSelectContent(select));
      const selected = content.querySelector('.custom-select-option.is-selected:not(:disabled)') || content.querySelector('.custom-select-option:not(:disabled)');
      if (selected) selected.focus({ preventScroll: true });
    } else {
      wrap.classList.remove('is-open');
      trigger.setAttribute('aria-expanded', 'false');
      content.hidden = true;
    }
  }
  function closeAllCustomSelects(except) {
    qsa('select.custom-select-native').forEach(select => {
      if (select !== except) setCustomSelectOpen(select, false);
    });
  }
  function chooseCustomSelectOption(select, index) {
    const option = select.options[index];
    if (!option || option.disabled) return;
    select.value = option.value;
    select.dispatchEvent(new Event('input', { bubbles: true }));
    select.dispatchEvent(new Event('change', { bubbles: true }));
    syncCustomSelect(select);
    setCustomSelectOpen(select, false);
    const trigger = select.__customSelect && select.__customSelect.querySelector('.custom-select-trigger');
    if (trigger && trigger.isConnected) trigger.focus({ preventScroll: true });
  }
  function focusSiblingCustomOption(current, dir) {
    const options = qsa('.custom-select-option:not(:disabled)', current.parentElement);
    const index = options.indexOf(current);
    const next = options[(index + dir + options.length) % options.length];
    if (next) next.focus({ preventScroll: true });
  }
  function getCustomSelectLabelElement(select) {
    const explicit = qsa('label').find(label => label.htmlFor === select.id);
    if (explicit) return explicit;
    const group = select.closest('.form-group');
    return group ? group.querySelector('label') : null;
  }
  function labelCustomSelect(select, trigger, content, id) {
    trigger.id = id + '-trigger';
    const valueId = id + '-value';
    const value = trigger.querySelector('.custom-select-value');
    if (value) value.id = valueId;
    const label = getCustomSelectLabelElement(select);
    if (label) {
      if (!label.id) label.id = id + '-label';
      trigger.removeAttribute('aria-label');
      trigger.setAttribute('aria-labelledby', label.id + ' ' + valueId);
    } else {
      trigger.removeAttribute('aria-labelledby');
      trigger.setAttribute('aria-label', select.getAttribute('aria-label') || getCustomSelectLabel(select));
    }
    content.setAttribute('aria-labelledby', trigger.id);
  }
  function enhanceCustomSelect(select) {
    if (!select || select.__customSelect || select.dataset.nativeSelect === 'true') return;

    const id = select.id || 'custom-select-' + (++customSelectUid);
    if (!select.id) select.id = id;

    const wrap = document.createElement('div');
    wrap.className = 'custom-select';
    wrap.dataset.customSelect = 'true';
    if (select.id === 'filterStatusSelect') wrap.classList.add('custom-select-filter');

    const trigger = document.createElement('button');
    trigger.type = 'button';
    trigger.className = 'custom-select-trigger';
    trigger.setAttribute('aria-haspopup', 'listbox');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('aria-controls', id + '-menu');
    trigger.innerHTML =
      '<span class="custom-select-value"></span>' +
      '<i class="fa-solid fa-chevron-down custom-select-icon" aria-hidden="true"></i>';

    const content = document.createElement('div');
    content.id = id + '-menu';
    content.className = 'custom-select-content';
    content.setAttribute('role', 'listbox');
    content.hidden = true;
    labelCustomSelect(select, trigger, content, id);

    wrap.appendChild(trigger);
    wrap.appendChild(content);
    select.insertAdjacentElement('afterend', wrap);
    select.classList.add('custom-select-native');
    select.setAttribute('aria-hidden', 'true');
    select.tabIndex = -1;
    select.__customSelect = wrap;
    wrap.__nativeSelect = select;

    trigger.addEventListener('click', () => setCustomSelectOpen(select, !wrap.classList.contains('is-open')));
    trigger.addEventListener('keydown', e => {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) {
        e.preventDefault();
        setCustomSelectOpen(select, true);
      }
    });
    content.addEventListener('click', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      chooseCustomSelectOption(select, parseInt(option.dataset.index, 10));
    });
    content.addEventListener('keydown', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      if (e.key === 'ArrowDown') { e.preventDefault(); focusSiblingCustomOption(option, 1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); focusSiblingCustomOption(option, -1); }
      else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); chooseCustomSelectOption(select, parseInt(option.dataset.index, 10)); }
      else if (e.key === 'Escape') { e.preventDefault(); setCustomSelectOpen(select, false); trigger.focus({ preventScroll: true }); }
    });
    select.addEventListener('change', () => syncCustomSelect(select));
    renderCustomSelectOptions(select);
  }
  function enhanceCustomSelects(root) {
    qsa('select:not(.custom-select-native)', root || document).forEach(enhanceCustomSelect);
  }
  function refreshCustomSelects(root) {
    enhanceCustomSelects(root);
    qsa('select.custom-select-native', root || document).forEach(renderCustomSelectOptions);
  }
  function positionOpenCustomSelects() {
    qsa('select.custom-select-native').forEach(placeCustomSelectContent);
  }
  function queueCustomSelectRefresh() {
    if (customSelectRefreshQueued) return;
    customSelectRefreshQueued = true;
    requestAnimationFrame(() => {
      customSelectRefreshQueued = false;
      refreshCustomSelects();
      positionOpenCustomSelects();
    });
  }
  function initCustomSelectObserver() {
    if (customSelectObserver || !document.body || typeof MutationObserver === 'undefined') return;
    customSelectObserver = new MutationObserver(mutations => {
      let shouldRefresh = false;
      for (const mutation of mutations) {
        const target = mutation.target;
        if (target && target.closest && target.closest('.custom-select')) continue;
        if (target && target.matches && target.matches('select')) {
          shouldRefresh = true;
          break;
        }
        for (const node of mutation.addedNodes || []) {
          if (node.nodeType !== 1) continue;
          if ((node.matches && node.matches('select')) || (node.querySelector && node.querySelector('select'))) {
            shouldRefresh = true;
            break;
          }
        }
        if (shouldRefresh) break;
      }
      if (shouldRefresh) queueCustomSelectRefresh();
    });
    customSelectObserver.observe(document.body, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['disabled', 'class', 'id', 'data-native-select']
    });
  }

  // Theme
  const THEME_ORDER = ['system', 'light', 'dark'];
  const themeMQ = window.matchMedia('(prefers-color-scheme: dark)');
  function resolveTheme(pref) {
    if (pref === 'dark') return 'dark';
    if (pref === 'light') return 'light';
    return themeMQ.matches ? 'dark' : 'light';
  }
  function applyTheme(pref) {
    const resolved = resolveTheme(pref);
    const root = document.documentElement;
    root.classList.toggle('dark', resolved === 'dark');
    root.dataset.themePref = pref;
    qsa('.theme-toggle').forEach(btn => {
      btn.dataset.theme = pref;
      const themeLabel = t('theme.status', t('theme.' + pref));
      btn.setAttribute('aria-label', themeLabel);
      btn.setAttribute('title', themeLabel);
    });
  }
  function getThemePref() {
    const saved = localStorage.getItem('kiro_theme');
    return THEME_ORDER.includes(saved) ? saved : 'system';
  }
  function initTheme() {
    applyTheme(getThemePref());
    themeMQ.addEventListener('change', () => {
      if (getThemePref() === 'system') applyTheme('system');
    });
  }
  function toggleTheme() {
    const cur = getThemePref();
    const next = THEME_ORDER[(THEME_ORDER.indexOf(cur) + 1) % THEME_ORDER.length];
    localStorage.setItem('kiro_theme', next);
    applyTheme(next);
  }

  // Privacy and email mask
  function initPrivacyMode() {
    const saved = localStorage.getItem('privacyMode');
    privacyModeEnabled = saved === null ? true : saved === 'true';
    const toggle = $('privacyModeToggle');
    if (toggle) toggle.checked = privacyModeEnabled;
  }
  function maskEmail(email) {
    if (!privacyModeEnabled || !email || email.indexOf('@') === -1) return email;
    const [local, domain] = email.split('@');
    const maskedLocal = local.length <= 2 ? local : local.substring(0, 2) + '***';
    const parts = domain.split('.');
    if (parts.length >= 2) {
      const tld = parts[parts.length - 1];
      const sld = parts[parts.length - 2];
      const maskedSld = sld.length <= 2 ? sld : sld.substring(0, 2) + '***';
      const subs = parts.slice(0, -2).map(s => s.length <= 2 ? s : s.substring(0, 2) + '***');
      return maskedLocal + '@' + [...subs, maskedSld, tld].join('.');
    }
    return maskedLocal + '@' + domain;
  }
  function getDisplayEmail(email, id) {
    const raw = email || (id ? id.substring(0, 12) + '...' : '-');
    return maskEmail(raw);
  }

  // Toast bridge
  const toast = function (msg, variant, opts) {
    if (typeof window.toast === 'function') return window.toast(msg, variant, opts);
    try { console.warn('[toast missing]', variant, msg); } catch (_) { }
    return function () {};
  };
  const toastPrimary = (msg, opts) => toast(msg, 'primary', opts);
  const toastWarning = (msg, opts) => toast(msg, 'warning', opts);
  const toastError = (msg, opts) => toast(msg, 'error', opts);

  // Modal helpers
  let modalScrollY = 0;
  let confirmResolve = null;
  const modalFocusStack = [];
  function lockModalScroll() {
    if (document.body.classList.contains('modal-open')) return;
    modalScrollY = window.scrollY || document.documentElement.scrollTop || 0;
    document.body.style.top = '-' + modalScrollY + 'px';
    document.body.classList.add('modal-open');
  }
  function unlockModalScrollIfIdle() {
    if (qsa('.modal.active').length > 0) return;
    if (!document.body.classList.contains('modal-open')) return;
    document.body.classList.remove('modal-open');
    document.body.style.top = '';
    window.scrollTo(0, modalScrollY);
  }
  function getModalFocusable(modal) {
    return qsa('a[href], button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])', modal)
      .filter(el => !el.closest('[hidden]'));
  }
  function prepareDialog(modal) {
    modal.setAttribute('role', 'dialog');
    modal.setAttribute('aria-modal', 'true');
    modal.setAttribute('aria-hidden', 'false');
    if (!modal.hasAttribute('tabindex')) modal.tabIndex = -1;
    const title = modal.querySelector('.modal-title');
    if (title) {
      if (!title.id) title.id = modal.id + 'Title';
      modal.setAttribute('aria-labelledby', title.id);
    }
  }
  function focusDialog(modal) {
    if (modal.contains(document.activeElement) && document.activeElement !== modal) return;
    const focusable = getModalFocusable(modal);
    const target = focusable[0] || modal;
    if (target && target.focus) target.focus({ preventScroll: true });
  }
  function trapDialogFocus(e) {
    const modal = e.currentTarget;
    if (e.key !== 'Tab' || !modal.classList.contains('active')) return;
    const focusable = getModalFocusable(modal);
    if (!focusable.length) {
      e.preventDefault();
      modal.focus({ preventScroll: true });
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus({ preventScroll: true });
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus({ preventScroll: true });
    }
  }
  function openDialog(id) {
    const modal = $(id);
    if (!modal) return;
    prepareDialog(modal);
    modalFocusStack.push({ id, el: document.activeElement });
    modal.removeEventListener('keydown', trapDialogFocus);
    modal.addEventListener('keydown', trapDialogFocus);
    modal.classList.add('active');
    lockModalScroll();
    focusDialog(modal);
    setTimeout(() => focusDialog(modal), 0);
  }
  function closeDialog(id) {
    const modal = $(id);
    if (!modal) return;
    modal.classList.remove('active');
    modal.setAttribute('aria-hidden', 'true');
    const stackIndex = modalFocusStack.map(item => item.id).lastIndexOf(id);
    const previous = stackIndex >= 0 ? modalFocusStack.splice(stackIndex, 1)[0].el : null;
    unlockModalScrollIfIdle();
    if (previous && previous.isConnected && previous.focus) {
      requestAnimationFrame(() => previous.focus({ preventScroll: true }));
    }
  }
  function bindDialogBackdropClose(id, closeFn) {
    const modal = $(id);
    if (!modal) return;
    let startedOnBackdrop = false;
    modal.addEventListener('pointerdown', e => {
      startedOnBackdrop = e.target === modal;
    });
    modal.addEventListener('click', e => {
      if (startedOnBackdrop && e.target === modal) closeFn();
      startedOnBackdrop = false;
    });
  }
  function closeConfirm(value) {
    if (!confirmResolve) return;
    const resolve = confirmResolve;
    confirmResolve = null;
    closeDialog('confirmModal');
    resolve(!!value);
  }
  function confirmAction(message, opts) {
    opts = opts || {};
    if (confirmResolve) closeConfirm(false);
    const modal = $('confirmModal');
    const title = $('confirmTitle');
    const msg = $('confirmMessage');
    const ok = $('confirmOk');
    const cancel = $('confirmCancel');
    const close = $('confirmClose');
    if (!modal || !title || !msg || !ok || !cancel || !close) {
      return Promise.resolve(false);
    }
    title.textContent = opts.title || t('common.confirm');
    msg.textContent = message || '';
    ok.textContent = opts.confirmText || t('common.confirm');
    cancel.textContent = opts.cancelText || t('common.cancel');
    ok.className = 'btn ' + (opts.variant === 'danger' ? 'btn-danger' : 'btn-primary');
    cancel.className = 'btn btn-secondary';
    ok.onclick = () => closeConfirm(true);
    cancel.onclick = () => closeConfirm(false);
    close.onclick = () => closeConfirm(false);
    const pending = new Promise(resolve => { confirmResolve = resolve; });
    openDialog('confirmModal');
    ok.focus({ preventScroll: true });
    return pending;
  }

  // Fetch wrapper
  function api(path, opts) {
    opts = opts || {};
    opts.headers = Object.assign({ 'X-Admin-Password': password }, opts.headers || {});
    if (opts.body && !opts.headers['Content-Type']) opts.headers['Content-Type'] = 'application/json';
    return fetch('/admin/api' + path, opts);
  }

  // Login
  function clearActivePassword() {
    sessionStorage.removeItem('admin_password');
    sessionStorage.removeItem('admin_login_time');
    localStorage.removeItem('admin_password');
    localStorage.removeItem('admin_login_time');
    password = '';
  }
  function getActiveLoginTime() {
    const storage = sessionStorage.getItem('admin_password') ? sessionStorage : localStorage;
    return parseInt(storage.getItem('admin_login_time') || '0', 10);
  }
  function setActivePassword(nextPassword, remember) {
    const now = Date.now().toString();
    password = nextPassword;
    sessionStorage.setItem('admin_password', nextPassword);
    sessionStorage.setItem('admin_login_time', now);
    if (remember) {
      localStorage.setItem('admin_password', nextPassword);
      localStorage.setItem('admin_login_time', now);
      localStorage.setItem('kiro_remember', '1');
      localStorage.setItem('kiro_remembered_pwd', nextPassword);
    } else {
      localStorage.removeItem('admin_password');
      localStorage.removeItem('admin_login_time');
      localStorage.removeItem('kiro_remember');
      localStorage.removeItem('kiro_remembered_pwd');
    }
  }
  async function tryAutoLogin() {
    if (!password) return;
    const loginTime = getActiveLoginTime();
    if (loginTime && Date.now() - loginTime > 72 * 3600 * 1000) {
      clearActivePassword();
      document.documentElement.classList.remove('preauth');
      $('loginPage').classList.remove('hidden');
      $('mainPage').classList.add('hidden');
      return;
    }
    try {
      const res = await api('/status');
      if (res.ok) { loadData(); }
      else {
        clearActivePassword();
        document.documentElement.classList.remove('preauth');
        $('loginPage').classList.remove('hidden');
        $('mainPage').classList.add('hidden');
      }
    } catch (e) {
      clearActivePassword();
      document.documentElement.classList.remove('preauth');
      $('loginPage').classList.remove('hidden');
      $('mainPage').classList.add('hidden');
    }
  }
  async function login() {
    password = $('pwdField').value;
    try {
      const res = await api('/status');
      if (res.ok) {
        const remember = $('rememberPwd');
        setActivePassword(password, !!(remember && remember.checked));
        showMain(); loadData();
      } else {
        toast(t('login.error'), 'error');
      }
    } catch (e) {
      toast(t('login.connectError'), 'error');
    }
  }
  function initRememberMe() {
    const remember = $('rememberPwd');
    const field = $('pwdField');
    if (!remember || !field) return;
    if (localStorage.getItem('kiro_remember') === '1') {
      remember.checked = true;
      const saved = localStorage.getItem('kiro_remembered_pwd');
      if (saved) field.value = saved;
    }
  }
  function logout() {
    clearActivePassword();
    location.reload();
  }
  function showMain() {
    $('loginPage').classList.add('hidden');
    $('mainPage').classList.remove('hidden');
  }

  // Data loaders
  async function loadData() {
    await Promise.all([loadStats(), loadAccounts(), loadSettings(), loadVersion()]);
    renderEndpointCode('claudeEndpoint', baseUrl + '/v1/messages');
    renderEndpointCode('openaiEndpoint', baseUrl + '/v1/chat/completions');
    renderEndpointCode('modelsEndpoint', baseUrl + '/v1/models');
    renderEndpointCode('statsEndpoint', baseUrl + '/v1/stats');
    const savedTab = localStorage.getItem('kiro_current_tab');
    if (savedTab && $('tab' + savedTab.charAt(0).toUpperCase() + savedTab.slice(1))) switchTab(savedTab);
    setTimeout(checkUpdate, 2000);
  }
  async function loadStats() {
    const res = await api('/status');
    const d = await res.json();
    $('statAccounts').textContent = d.accounts || 0;
    $('statRequests').textContent = d.totalRequests || 0;
    $('statSuccess').textContent = d.successRequests || 0;
    $('statFailed').textContent = d.failedRequests || 0;
    $('statTokens').textContent = formatNum(d.totalTokens || 0);
    $('statCredits').textContent = (d.totalCredits || 0).toFixed(1);
    const set = (id, v) => { const el = $(id); if (el) el.textContent = v; };
    set('statStandby', d.standbyAccounts || 0);
    set('statBanned', d.totalBanned || 0);
    set('statExhausted', d.totalExhausted || 0);
    set('statTodayBanned', d.todayBanned || 0);
    set('statTodayExhausted', d.todayExhausted || 0);
  }
  async function loadAccounts() {
    const res = await api('/accounts');
    accountsData = await res.json();
    await refreshGroupOptions();
    updateGroupFilterOptions();
    applySavedAccountFilters();
    updateGroupFilterOptions();
    renderAccounts();
  }

  // Account list
  function isAccountBanned(a) {
    return !!(a.banStatus && a.banStatus !== 'ACTIVE');
  }
  function isAccountExhausted(a) {
    return (a.usageLimit || 0) > 0 && (a.usageCurrent || 0) >= (a.usageLimit || 0);
  }
  function isAccountAvailable(a) {
    return a.enabled && !a.standby && !isAccountBanned(a) && !isAccountExhausted(a);
  }
  function getFilteredAccounts() {
    return applySort(accountsData.filter(a => {
      if (filterStatus === 'available' && !isAccountAvailable(a)) return false;
      if (filterStatus === 'enabled' && (!a.enabled || a.standby || isAccountBanned(a) || isAccountExhausted(a))) return false;
      if (filterStatus === 'disabled' && (a.enabled || isAccountBanned(a))) return false;
      if (filterStatus === 'standby' && (!a.standby || isAccountBanned(a))) return false;
      if (filterStatus === 'banned' && !isAccountBanned(a)) return false;
      if (filterStatus === 'exhausted' && (!isAccountExhausted(a) || isAccountBanned(a))) return false;
      if (filterGroup !== 'all') {
        const groups = Array.isArray(a.groups) && a.groups.length ? a.groups : ['default'];
        if (!groups.includes(filterGroup)) return false;
      }
      if (filterKeyword) {
        const kw = filterKeyword.toLowerCase();
        const hay = [a.email, a.nickname, a.userId, a.id, ...(a.groups || [])].filter(Boolean).join(' ').toLowerCase();
        if (!hay.includes(kw)) return false;
      }
      return true;
    }));
  }
  function onFilterChange() {
    filterKeyword = $('filterSearch') ? $('filterSearch').value : '';
    filterStatus = $('filterStatusSelect') ? $('filterStatusSelect').value : 'all';
    filterGroup = $('filterGroupSelect') ? $('filterGroupSelect').value : 'all';
    sortField = $('sortSelect') ? $('sortSelect').value : '';
    try {
      localStorage.setItem('filterKeyword', filterKeyword);
      localStorage.setItem('filterStatus', filterStatus);
      localStorage.setItem('filterGroup', filterGroup);
      localStorage.setItem('sortField', sortField);
    } catch (e) { }
    renderAccounts();
  }
  function clearFilters() {
    filterKeyword = '';
    filterStatus = 'all';
    filterGroup = 'all';
    sortField = '';
    const search = $('filterSearch');
    const status = $('filterStatusSelect');
    const group = $('filterGroupSelect');
    const sort = $('sortSelect');
    if (search) search.value = '';
    if (status) status.value = 'all';
    if (group) group.value = 'all';
    if (sort) sort.value = '';
    try {
      localStorage.removeItem('filterKeyword');
      localStorage.removeItem('filterStatus');
      localStorage.removeItem('filterGroup');
      localStorage.removeItem('sortField');
    } catch (e) { }
    refreshCustomSelects();
    renderAccounts();
  }
  function switchStatusTab(status) {
    filterStatus = status || 'all';
    const select = $('filterStatusSelect');
    if (select) {
      select.value = filterStatus;
      syncCustomSelect(select);
    }
    try { localStorage.setItem('filterStatus', filterStatus); } catch (e) { }
    updateStatusTabCounts();
    renderAccounts();
  }
  function applySavedAccountFilters() {
    try {
      filterKeyword = localStorage.getItem('filterKeyword') || '';
      filterStatus = localStorage.getItem('filterStatus') || 'all';
      filterGroup = localStorage.getItem('filterGroup') || 'all';
      sortField = localStorage.getItem('sortField') || '';
    } catch (e) { }
    const search = $('filterSearch');
    const status = $('filterStatusSelect');
    const group = $('filterGroupSelect');
    const sort = $('sortSelect');
    if (search) search.value = filterKeyword;
    if (status) status.value = filterStatus;
    if (group) group.value = filterGroup;
    if (sort) sort.value = sortField;
  }
  function applySort(list) {
    if (!sortField) return list;
    const desc = sortField.charAt(0) === '-';
    const field = desc ? sortField.slice(1) : sortField;
    return list.slice().sort((a, b) => {
      let va = a[field], vb = b[field];
      if (field === 'email') {
        va = String(va || '').toLowerCase();
        vb = String(vb || '').toLowerCase();
        if (va < vb) return desc ? 1 : -1;
        if (va > vb) return desc ? -1 : 1;
        return 0;
      }
      va = Number(va) || 0;
      vb = Number(vb) || 0;
      return desc ? vb - va : va - vb;
    });
  }
  function updateGroupFilterOptions() {
    const select = $('filterGroupSelect');
    if (!select) return;
    const cur = filterGroup || select.value || 'all';
    const groupSet = new Set(['default']);
    (allGroups || []).forEach(g => groupSet.add(g));
    (accountsData || []).forEach(a => (a.groups || []).forEach(g => groupSet.add(g)));
    select.innerHTML = '<option value="all">' + escapeHtml(t('filter.allGroups')) + '</option>' +
      Array.from(groupSet).filter(Boolean).sort().map(g => '<option value="' + escapeAttr(g) + '">' + escapeHtml(g) + '</option>').join('');
    select.value = Array.from(select.options).some(o => o.value === cur) ? cur : 'all';
    filterGroup = select.value;
    refreshCustomSelects(select.parentElement || document);
  }
  function updateStatusTabCounts() {
    const counts = {
      all: accountsData.length,
      available: accountsData.filter(isAccountAvailable).length,
      enabled: accountsData.filter(a => a.enabled && !a.standby && !isAccountBanned(a) && !isAccountExhausted(a)).length,
      disabled: accountsData.filter(a => !a.enabled && !isAccountBanned(a)).length,
      standby: accountsData.filter(a => a.standby && !isAccountBanned(a)).length,
      banned: accountsData.filter(isAccountBanned).length,
      exhausted: accountsData.filter(a => isAccountExhausted(a) && !isAccountBanned(a)).length
    };
    const select = $('filterStatusSelect');
    if (!select) return;
    Array.from(select.options).forEach(opt => {
      const key = opt.value;
      const labelKey = opt.dataset.i18n || ('filter.' + key);
      opt.textContent = t(labelKey) + (key in counts ? ' (' + counts[key] + ')' : '');
    });
    qsa('.status-tab').forEach(tab => {
      const key = tab.dataset.status;
      const labelKey = tab.dataset.i18n || ('filter.' + key);
      tab.textContent = t(labelKey) + (key in counts ? ' (' + counts[key] + ')' : '');
      tab.classList.toggle('active', key === filterStatus);
      tab.setAttribute('aria-selected', String(key === filterStatus));
    });
    refreshCustomSelects(select.parentElement || document);
  }
  function toggleSelectAll(checked) {
    const filtered = getFilteredAccounts();
    if (checked) filtered.forEach(a => selectedAccounts.add(a.id));
    else selectedAccounts.clear();
    renderAccounts();
    updateBatchBar();
  }
  function toggleSelectAccount(id) {
    if (selectedAccounts.has(id)) selectedAccounts.delete(id);
    else selectedAccounts.add(id);
    updateBatchBar();
  }
  function updateBatchBar() {
    const bar = $('batchBar');
    const count = selectedAccounts.size;
    const cb = $('selectAllCheckbox');
    if (cb) {
      const filtered = getFilteredAccounts();
      const selectedFiltered = filtered.filter(a => selectedAccounts.has(a.id)).length;
      cb.checked = filtered.length > 0 && selectedFiltered === filtered.length;
      cb.indeterminate = selectedFiltered > 0 && selectedFiltered < filtered.length;
    }
    if (count > 0) {
      bar.classList.remove('hidden');
      $('batchCount').textContent = t('batch.selected', count);
    } else {
      bar.classList.add('hidden');
    }
  }

  function formatSubscriptionLabel(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return t('subscription.power');
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return t('subscription.proPlus');
    if (s.includes('PRO')) return t('subscription.pro');
    if (s.includes('FREE')) return t('subscription.free');
    return type || t('subscription.free');
  }
  function getSubBadge(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return '<span class="badge badge-power">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return '<span class="badge badge-proplus">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO')) return '<span class="badge badge-pro">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    return '<span class="badge badge-free">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
  }
  function getTrialBadge(a) {
    if (a.trialStatus === 'ACTIVE' && a.trialUsageLimit > 0) {
      return '<span class="badge badge-trial">' + escapeHtml(t('accounts.trial')) + '</span>';
    }
    return '';
  }
  function formatTrialExpiry(ts) {
    if (!ts) return '';
    const date = new Date(ts * 1000);
    const diffDays = Math.ceil((date - new Date()) / (1000 * 60 * 60 * 24));
    if (diffDays < 0) return '(' + t('accounts.trialExpired') + ')';
    if (diffDays === 0) return '(' + t('accounts.trialToday') + ')';
    if (diffDays <= 7) return '(' + diffDays + t('accounts.trialDays') + ')';
    return '';
  }
  function formatAuthMethod(method) {
    if (!method) return '-';
    const normalized = String(method).toLowerCase();
    if (normalized === 'idc') return t('auth.enterprise');
    if (normalized === 'social') return t('auth.social');
    if (normalized === 'builderid') return 'BuilderID';
    if (normalized === 'github') return t('local.providerGithub');
    if (normalized === 'google') return t('local.providerGoogle');
    return method;
  }
  function getStatusBadge(a) {
    const out = [];
    const isBanned = a.banStatus && a.banStatus !== 'ACTIVE';
    if (isBanned) {
      if (a.banStatus === 'BANNED') out.push('<span class="badge badge-banned">' + escapeHtml(t('accounts.banned')) + '</span>');
      else if (a.banStatus === 'SUSPENDED') out.push('<span class="badge badge-suspended">' + escapeHtml(t('accounts.suspended')) + '</span>');
      out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    } else {
      if (!a.hasToken)
        out.push('<span class="badge badge-error">' + escapeHtml(t('accounts.noToken')) + '</span>');
      else if (a.expiresAt && a.expiresAt < Date.now() / 1000)
        out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.expired')) + '</span>');
      else
        out.push('<span class="badge badge-success">' + escapeHtml(t('accounts.normal')) + '</span>');
      out.push(a.enabled
        ? '<span class="badge badge-info">' + escapeHtml(t('accounts.enabled')) + '</span>'
        : '<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    }
    return out.join('');
  }
  function formatTokenExpiry(ts) {
    if (!ts) return '-';
    const diff = ts - Date.now() / 1000;
    if (diff <= 0) return t('time.expired');
    if (diff < 3600) return Math.floor(diff / 60) + t('time.minutes');
    if (diff < 86400) return Math.floor(diff / 3600) + t('time.hours');
    return Math.floor(diff / 86400) + t('time.days');
  }
  function formatLastUsed(ts) {
    if (!ts) return t('time.never');
    const diff = Date.now() / 1000 - ts;
    if (diff < 60) return t('time.justNow');
    if (diff < 3600) return Math.floor(diff / 60) + t('time.minutesAgo');
    if (diff < 86400) return Math.floor(diff / 3600) + t('time.hoursAgo');
    if (diff < 2592000) return Math.floor(diff / 86400) + t('time.daysAgo');
    return Math.floor(diff / 2592000) + t('time.monthsAgo');
  }
  function formatNum(n) {
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return n.toString();
  }
  function applyUsageBars(root) {
    qsa('.usage-fill[data-usage-pct]', root).forEach(el => {
      const pct = Math.max(0, Math.min(100, parseFloat(el.dataset.usagePct) || 0));
      el.style.width = pct + '%';
    });
  }

  function renderAccounts() {
    const container = $('accountsList');
    if (!container) return;
    updateStatusTabCounts();
    const filtered = getFilteredAccounts();
    if (filtered.length === 0) {
      container.innerHTML = '<div class="empty-state">' + escapeHtml(t('accounts.empty')) + '</div>';
      updateBatchBar();
      return;
    }
    container.innerHTML = filtered.map(a => {
      const usagePct = (a.usagePercent || 0) * 100;
      const usageClass = usagePct > 90 ? 'critical' : usagePct > 70 ? 'high' : '';
      const trialPct = (a.trialUsagePercent || 0) * 100;
      const trialClass = trialPct > 90 ? 'critical' : trialPct > 70 ? 'high' : '';
      const isSelected = selectedAccounts.has(a.id);
      const weight = Number(a.weight) || 0;
      const weightBadge = weight >= 2 ? '<span class="badge badge-warning">' + escapeHtml(t('accounts.weightShort')) + ':' + weight + '</span>' : '';
      const overageBadge = a.allowOverage ? '<span class="badge badge-warning">' + escapeHtml(t('accounts.overage')) + ':' + (a.overageWeight || 1) + '</span>' : '';
      const banned = a.banStatus && a.banStatus !== 'ACTIVE';
      const exhaustedBadge = isAccountExhausted(a) ? '<span class="badge badge-error">' + escapeHtml(t('accounts.exhausted')) + '</span>' : '';
      const standbyBadge = a.standby ? '<span class="badge badge-warning">' + escapeHtml(t('filter.standby')) + '</span>' : '';
      const groupBadges = (Array.isArray(a.groups) && a.groups.length ? a.groups : ['default'])
        .filter(Boolean)
        .map(g => '<span class="badge badge-group">' + escapeHtml(t('accounts.group')) + ':' + escapeHtml(g) + '</span>')
        .join('');
      const idAttr = escapeAttr(a.id);
      const displayEmail = getDisplayEmail(a.email, a.id);
      const selectLabel = t('accounts.selectAccount', displayEmail);
      const weightOptions = [0, 1, 2, 3, 4, 5]
        .map(w => '<option value="' + w + '"' + (weight === w ? ' selected' : '') + '>' + w + '</option>')
        .join('');

      const refreshSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>';
      const userSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>';
      const copySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';

      return '' +
        '<div class="account-card' + (isSelected ? ' selected' : '') + '" data-id="' + idAttr + '">' +
        '<div class="account-header">' +
        '<div class="account-info">' +
        '<input type="checkbox" class="account-checkbox" ' + (isSelected ? 'checked' : '') + ' data-id="' + idAttr + '" aria-label="' + escapeAttr(selectLabel) + '" />' +
        '<div class="account-info-text">' +
        '<div class="account-email">' + escapeHtml(displayEmail) + '</div>' +
        '<div class="account-meta">' +
        getSubBadge(a.subscriptionType) +
        getTrialBadge(a) +
        weightBadge +
        overageBadge +
        exhaustedBadge +
        standbyBadge +
        groupBadges +
        '<span class="badge badge-info">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + '</span>' +
        getStatusBadge(a) +
        '</div>' +
        '</div>' +
        '</div>' +
        '<div class="account-actions">' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="refresh" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refresh')) + '">' + refreshSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="detail" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.detail')) + '">' + userSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="copyJSON" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.copyJSON')) + '">' + copySvg + '</button>' +
        (banned ? '' :
          '<button class="btn btn-sm ' + (a.enabled ? 'btn-outline' : 'btn-primary') + '" data-action="toggle" data-id="' + idAttr + '" data-enabled="' + (!a.enabled) + '">' +
          escapeHtml(a.enabled ? t('accounts.disable') : t('accounts.enable')) +
          '</button>') +
        '<button class="btn btn-sm btn-secondary" data-action="test" data-id="' + idAttr + '" id="test-' + idAttr + '">' + escapeHtml(t('accounts.test')) + '</button>' +
        '<button class="btn btn-sm btn-danger" data-action="delete" data-id="' + idAttr + '">' + escapeHtml(t('accounts.delete')) + '</button>' +
        '</div>' +
        '</div>' +
        (a.usageLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.mainQuota')) + '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + usageClass + '" data-usage-pct="' + escapeAttr(usagePct) + '"></div></div>' +
          '<div class="usage-text"><span>' + (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0) + '</span><span>' + usagePct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        (a.trialUsageLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.trialQuota')) + ' ' + escapeHtml(formatTrialExpiry(a.trialExpiresAt)) + '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + trialClass + '" data-usage-pct="' + escapeAttr(trialPct) + '"></div></div>' +
          '<div class="usage-text"><span>' + (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + (a.trialUsageLimit != null ? a.trialUsageLimit.toFixed(0) : 0) + '</span><span>' + trialPct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        '<div class="account-stats">' +
        '<div class="account-stat"><div class="account-stat-value">' + (a.requestCount || 0) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.requests')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + formatNum(a.totalTokens || 0) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.tokens')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + (a.totalCredits || 0).toFixed(1) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.credits')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + escapeHtml(formatTokenExpiry(a.expiresAt)) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.expiry')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + escapeHtml(formatLastUsed(a.lastUsed)) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.lastUsed')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value"><select class="quick-weight-select" data-native-select="true" data-id="' + idAttr + '" aria-label="' + escapeAttr(t('accounts.weight')) + '">' + weightOptions + '</select></div><div class="account-stat-label">' + escapeHtml(t('accounts.weight')) + '</div></div>' +
        '</div>' +
        '</div>';
    }).join('');
    applyUsageBars(container);
    enhanceCustomSelects(container);
    updateBatchBar();
  }

  // Account actions
  async function refreshAccount(id, card) {
    if (card) card.classList.add('loading');
    try {
      const res = await api('/accounts/' + id + '/refresh', { method: 'POST' });
      const d = await res.json();
      if (d.success) loadAccounts();
      else toastError(t('accounts.refreshFailed') + ': ' + (d.error || ''));
    } catch (e) {
      toastError(t('accounts.refreshFailed'));
    }
    if (card) card.classList.remove('loading');
  }
  async function toggleAccount(id, enabled) {
    await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ enabled }) });
    loadAccounts();
  }
  async function deleteAccount(id) {
    const ok = await confirmAction(t('accounts.confirmDelete'), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/accounts/' + id, { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('accounts.deleteSuccess'), 'danger', { icon: 'fa-solid fa-trash' });
      loadAccounts(); loadStats();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function copyAccountJSON(id, btn) {
    try {
      const res = await api('/accounts/' + id + '/full');
      if (!res.ok) throw new Error('Failed');
      const a = await res.json();
      const { clientId, clientSecret, accessToken, refreshToken } = a;
      const json = JSON.stringify({ clientId, clientSecret, accessToken, refreshToken }, null, 2);
      await copyText(json);
      flashCopySuccess(btn);
      toastPrimary(t('accounts.copyJSONSuccess'));
    } catch (e) {
      toastError(t('common.failed'));
    }
  }
  async function quickSetWeight(id, value) {
    const weight = Math.max(0, Math.min(10, parseInt(value, 10) || 0));
    try {
      const res = await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ weight }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      const acc = accountsData.find(a => a.id === id);
      if (acc) acc.weight = weight;
      renderAccounts();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
      loadAccounts();
    }
  }
  function flashCopySuccess(btn) {
    if (!btn) return;
    const html = btn.innerHTML, cls = btn.className;
    btn.disabled = true;
    btn.className = 'btn btn-icon btn-sm btn-success';
    btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>';
    setTimeout(() => { btn.disabled = false; btn.className = cls; btn.innerHTML = html; }, 800);
  }

  // Batch actions
  async function batchAction(action) {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmKey = 'batch.confirm' + action.charAt(0).toUpperCase() + action.slice(1);
    const ok = await confirmAction(t(confirmKey, ids.length), {
      title: t('common.confirm'),
      confirmText: t('common.confirm'),
      variant: action === 'disable' ? 'danger' : 'primary'
    });
    if (!ok) return;
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/batch', { method: 'POST', body: JSON.stringify({ ids, action }) });
      const d = await res.json();
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      dismiss();
      if (action === 'refresh') {
        toast(t('batch.refreshResult', d.refreshed || 0, d.failed || 0), d.failed ? 'warning' : 'success');
      } else if (action === 'enable') {
        toast(t('batch.enableResult', d.count || ids.length), 'success');
      } else if (action === 'disable') {
        toast(t('batch.disableResult', d.count || ids.length), 'success');
      } else {
        toast(t('batch.done'), 'success');
      }
      selectedAccounts.clear();
      updateBatchBar();
      loadAccounts(); loadStats();
    } catch (e) {
      dismiss();
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function batchSetGroup() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    await refreshGroupOptions();
    if (!allGroups.length) {
      toastWarning(t('batchGroupSelect.noGroups'));
      return;
    }
    openBatchGroupSelectModal(ids);
  }
  function renderGroupCheckboxes(groups, selected, className) {
    const selectedSet = new Set((selected || []).map(String));
    if (!groups.length) {
      return '<p class="empty-state compact">' + escapeHtml(t('batchGroupSelect.noGroups')) + '</p>';
    }
    return '<div class="group-checkbox-list">' + groups.map(g => {
      const value = String(g || '').trim();
      if (!value) return '';
      return '<label class="group-checkbox-row">' +
        '<input type="checkbox" class="' + escapeAttr(className) + '" value="' + escapeAttr(value) + '"' + (selectedSet.has(value) ? ' checked' : '') + ' />' +
        '<span>' + escapeHtml(value) + '</span>' +
        '</label>';
    }).join('') + '</div>';
  }
  function openBatchGroupSelectModal(ids) {
    currentBatchGroupIds = ids.slice();
    const body = $('batchGroupSelectBody');
    if (!body) return;
    body.innerHTML =
      '<p class="modal-hint">' + escapeHtml(t('batchGroupSelect.hint', ids.length)) + '</p>' +
      renderGroupCheckboxes(allGroups || [], [], 'batch-group-checkbox') +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" type="button" data-batch-group-action="cancel">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-primary" type="button" data-batch-group-action="save">' + escapeHtml(t('common.save')) + '</button>' +
      '</div>';
    openDialog('batchGroupSelectModal');
  }
  function closeBatchGroupSelectModal() {
    closeDialog('batchGroupSelectModal');
    currentBatchGroupIds = null;
  }
  async function saveBatchGroupFromModal() {
    const ids = currentBatchGroupIds ? currentBatchGroupIds.slice() : [];
    if (!ids.length) return;
    const groups = qsa('.batch-group-checkbox:checked', $('batchGroupSelectBody')).map(cb => cb.value);
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/batch', { method: 'POST', body: JSON.stringify({ ids, action: 'setGroup', groups }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      dismiss();
      toast(t('batch.setGroupResult', d.count || ids.length, groups.join(', ') || 'default'), 'success');
      closeBatchGroupSelectModal();
      selectedAccounts.clear();
      updateBatchBar();
      await loadAccounts();
      loadStats();
    } catch (e) {
      dismiss();
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function batchClearGroup() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const ok = await confirmAction(t('batch.confirmClearGroup', ids.length), {
      title: t('batch.clearGroup'),
      confirmText: t('batch.clearGroup'),
      variant: 'danger'
    });
    if (!ok) return;
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/batch', { method: 'POST', body: JSON.stringify({ ids, action: 'setGroup', groups: [] }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      dismiss();
      toast(t('batch.clearGroupResult', d.count || ids.length), 'success');
      selectedAccounts.clear();
      updateBatchBar();
      await loadAccounts();
      loadStats();
    } catch (e) {
      dismiss();
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function batchSetStandby() {
    await batchStandbyAction('setStandby');
  }
  async function batchRemoveStandby() {
    await batchStandbyAction('removeStandby');
  }
  async function batchStandbyAction(action) {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const isSet = action === 'setStandby';
    const ok = await confirmAction(t(isSet ? 'batch.confirmSetStandby' : 'batch.confirmRemoveStandby', ids.length), {
      title: t(isSet ? 'batch.setStandby' : 'batch.removeStandby'),
      confirmText: t(isSet ? 'batch.setStandby' : 'batch.removeStandby'),
      variant: isSet ? 'primary' : 'danger'
    });
    if (!ok) return;
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/batch', { method: 'POST', body: JSON.stringify({ ids, action }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      dismiss();
      toast(t(isSet ? 'batch.setStandbyResult' : 'batch.removeStandbyResult', d.count || ids.length), 'success');
      selectedAccounts.clear();
      updateBatchBar();
      loadAccounts();
      loadStats();
    } catch (e) {
      dismiss();
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function batchDelete() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const ok = await confirmAction(t('batch.confirmDelete', ids.length), {
      title: t('batch.delete'),
      confirmText: t('batch.delete'),
      variant: 'danger'
    });
    if (!ok) return;
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    let success = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id, { method: 'DELETE' });
        const d = await res.json().catch(() => ({}));
        if (res.ok && d.success !== false) success++;
        else fail++;
      } catch (e) {
        fail++;
      }
    }
    dismiss();
    toast(t('batch.deleteResult', success, fail), fail ? 'warning' : 'success');
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts();
    loadStats();
  }
  async function batchRefreshModels() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmRefreshModels', ids.length), {
      title: t('models.refreshAll'),
      confirmText: t('common.confirm')
    });
    if (!confirmed) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    let ok = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
        const d = await res.json();
        if (d.success) ok++; else fail++;
      } catch { fail++; }
    }
    dismiss();
    toast(t('batch.refreshModelsResult', ok, fail), fail ? 'warning' : 'success');
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts();
  }
  async function refreshAllModels() {
    const ok = await confirmAction(t('models.confirmRefreshAll'), {
      title: t('models.refreshAll'),
      confirmText: t('models.refreshAll')
    });
    if (!ok) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      toast(t('models.refreshAllDone', d.refreshed || 0), 'success');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }
  async function refreshAccountModels(id) {
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      if (d.success) toast(t('detail.refreshModelCache') + ' · ' + (d.count || 0), 'success');
      else toast(t('common.failed') + (d.error ? ': ' + d.error : ''), 'error');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }

  // Detail modal
  function detailItem(label, value) {
    return '<div class="detail-item"><div class="detail-label">' + escapeHtml(label) + '</div><div class="detail-value">' + escapeHtml(value) + '</div></div>';
  }
  function detailItemHtml(label, valueHtml) {
    return '<div class="detail-item"><div class="detail-label">' + escapeHtml(label) + '</div><div class="detail-value">' + valueHtml + '</div></div>';
  }
  function groupBadgeList(groups) {
    const list = (Array.isArray(groups) && groups.length ? groups : ['default']).filter(Boolean);
    return list.length
      ? list.map(g => '<span class="badge badge-group">' + escapeHtml(g) + '</span>').join('')
      : '<span class="muted-text">—</span>';
  }
  function detailTabButton(tab, label, active) {
    return '<button class="detail-tab' + (active ? ' active' : '') + '" type="button" data-detail-tab="' + escapeAttr(tab) + '">' + escapeHtml(label) + '</button>';
  }
  function detailTabContent(tab, html, active) {
    return '<div class="detail-tab-content' + (active ? ' active' : '') + '" data-detail-content="' + escapeAttr(tab) + '">' + html + '</div>';
  }
  async function showDetail(id) {
    const a = accountsData.find(x => x.id === id);
    if (!a) return;
    await refreshGroupOptions();
    const idAttr = escapeAttr(id);
    const groupsJson = escapeAttr(JSON.stringify(a.groups || []));
    const basicTab =
      '<div class="detail-grid">' +
      detailItem(t('detail.email'), getDisplayEmail(a.email, null)) +
      detailItem(t('detail.userId'), a.userId || '-') +
      detailItem(t('detail.authMethod'), formatAuthMethod(a.provider || a.authMethod)) +
      detailItem(t('detail.region'), a.region || 'us-east-1') +
      detailItem(t('detail.status'), a.enabled ? t('accounts.enabled') : t('accounts.disabled')) +
      '</div>';

    const configTab =
      '<div class="detail-section"><h4>' + escapeHtml(t('detail.machineId')) + '</h4><div class="machine-id-row">' +
      '<input type="text" id="machineIdInput" value="' + escapeAttr(a.machineId || '') + '" placeholder="UUID" />' +
      '<button class="btn btn-sm btn-outline" id="generateMachineIdBtn" type="button">' + escapeHtml(t('detail.generate')) + '</button>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveMachineId" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.weight')) + '</h4>' +
      '<div class="form-group">' +
      '<input type="number" id="weightInput" value="' + (a.weight || 0) + '" min="0" max="10" />' +
      '<small>' + escapeHtml(t('detail.weightHint')) + '</small>' +
      '</div>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveWeight" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.overage')) + '</h4>' +
      '<div class="form-group">' +
      '<label class="flex items-center gap-2"><span class="switch"><input type="checkbox" id="allowOverageInput" ' + (a.allowOverage ? 'checked' : '') + ' /><span class="slider"></span></span><span>' + escapeHtml(t('detail.allowOverage')) + '</span></label>' +
      '</div>' +
      '<div class="form-group">' +
      '<label>' + escapeHtml(t('detail.overageWeight')) + '</label>' +
      '<input type="number" id="overageWeightInput" value="' + (a.overageWeight || 1) + '" min="1" max="10" />' +
      '<small>' + escapeHtml(t('detail.overageHint')) + '</small>' +
      '</div>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveOverage" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.group')) + '</h4>' +
      '<div class="group-badge-list">' + groupBadgeList(a.groups || []) + '</div>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="selectGroups" data-id="' + idAttr + '" data-groups="' + groupsJson + '" type="button">' + escapeHtml(t('detail.selectGroups')) + '</button>' +
      '<p class="help-block">' + escapeHtml(t('detail.groupHint')) + '</p></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.proxyURL')) + '</h4><div class="machine-id-row">' +
      '<input type="text" id="proxyURLInput" value="' + escapeAttr(a.proxyURL || '') + '" placeholder="socks5://host:port" />' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveProxyURL" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div><p class="help-block">' + escapeHtml(t('detail.proxyHint')) + '</p></div>';

    const subscriptionTab =
      '<div class="detail-grid">' +
      detailItem(t('detail.subscriptionType'), a.subscriptionTitle || (a.subscriptionType ? formatSubscriptionLabel(a.subscriptionType) : '-')) +
      detailItem(t('detail.tokenExpiry'), a.expiresAt ? new Date(a.expiresAt * 1000).toLocaleString() : '-') +
      detailItem(t('detail.mainQuota'), (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0)) +
      detailItem(t('detail.resetDate'), a.nextResetDate || '-') +
      (a.trialUsageLimit > 0 ?
        detailItem(t('detail.trialQuota'), (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + a.trialUsageLimit.toFixed(0)) +
        detailItem(t('detail.trialStatus'), a.trialStatus || '-') +
        detailItem(t('detail.trialExpiry'), a.trialExpiresAt ? new Date(a.trialExpiresAt * 1000).toLocaleString() : '-')
        : '') +
      '</div>';

    const statsTab =
      '<div class="detail-grid">' +
      detailItem(t('detail.requestCount'), a.requestCount || 0) +
      detailItem(t('detail.errorCount'), a.errorCount || 0) +
      detailItem(t('detail.totalTokens'), formatNum(a.totalTokens || 0)) +
      detailItem(t('detail.totalCredits'), (a.totalCredits || 0).toFixed(2)) +
      detailItem(t('accounts.lastUsed'), formatLastUsed(a.lastUsed)) +
      '</div>';

    const standbyTab =
      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.standbyStatus')) + '</h4>' +
      '<div class="detail-grid">' +
      detailItemHtml(t('detail.standbyMode'), a.standby
        ? '<span class="standby-active">' + escapeHtml(t('detail.standbyActive')) + '</span>'
        : '<span class="muted-text">' + escapeHtml(t('detail.standbyInactive')) + '</span>') +
      (a.standbyTime ? detailItem(t('detail.standbyTime'), new Date(a.standbyTime * 1000).toLocaleString()) : '') +
      '</div>' +
      '<p class="help-block">' + escapeHtml(t('detail.standbyHint')) + '</p>' +
      '<button class="btn btn-sm ' + (a.standby ? 'btn-secondary' : 'btn-primary') + '" data-detail-action="toggleStandby" data-id="' + idAttr + '" data-standby="' + (!a.standby) + '" type="button">' +
      escapeHtml(a.standby ? t('detail.removeStandby') : t('detail.setStandby')) +
      '</button>' +
      '</div>';

    const modelsTab =
      '<div class="detail-section">' +
      '<div class="detail-toolbar">' +
      '<button class="btn btn-sm btn-outline" data-detail-action="loadModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.loadModels')) + '</button>' +
      '<button class="btn btn-sm btn-outline" data-detail-action="refreshModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.refreshModelCache')) + '</button>' +
      '</div>' +
      '<div id="modelsList" class="model-list"></div>' +
      '</div>';

    $('detailBody').innerHTML =
      '<div class="detail-tabs">' +
      detailTabButton('basic', t('detail.tabBasic'), true) +
      detailTabButton('config', t('detail.tabConfig'), false) +
      detailTabButton('subscription', t('detail.tabSubscription'), false) +
      detailTabButton('stats', t('detail.tabStats'), false) +
      detailTabButton('standby', t('detail.tabStandby'), false) +
      detailTabButton('models', t('detail.tabModels'), false) +
      '</div>' +
      detailTabContent('basic', basicTab, true) +
      detailTabContent('config', configTab, false) +
      detailTabContent('subscription', subscriptionTab, false) +
      detailTabContent('stats', statsTab, false) +
      detailTabContent('standby', standbyTab, false) +
      detailTabContent('models', modelsTab, false);

    const modal = $('detailModal');
    if (modal) modal.dataset.accountId = id;
    if (modal && modal.classList.contains('active')) focusDialog(modal);
    else openDialog('detailModal');
  }
  function switchDetailTab(tab) {
    qsa('.detail-tab', $('detailBody')).forEach(btn => btn.classList.toggle('active', btn.dataset.detailTab === tab));
    qsa('.detail-tab-content', $('detailBody')).forEach(c => c.classList.toggle('active', c.dataset.detailContent === tab));
    if (tab === 'models') {
      const accountId = $('detailModal') && $('detailModal').dataset.accountId;
      const modelsList = $('modelsList');
      if (accountId && modelsList && !modelsList.dataset.loaded) {
        modelsList.dataset.loaded = 'true';
        loadModels(accountId);
      }
    }
  }
  async function loadModels(id) {
    const c = $('modelsList');
    c.innerHTML = '<p class="empty-state">' + escapeHtml(t('detail.loading')) + '</p>';
    try {
      const res = await api('/accounts/' + id + '/models');
      const d = await res.json();
      if (d.success && d.models) {
        const sorted = d.models.slice().sort((a, b) => {
          if (a.modelId === 'auto') return -1;
          if (b.modelId === 'auto') return 1;
          return (a.rateMultiplier || 1) - (b.rateMultiplier || 1);
        });
        c.innerHTML = sorted.map(m => {
          const ratio = m.rateMultiplier || 1;
          return '<div class="model-item">' +
            '<div class="model-name">' + escapeHtml(m.modelId) + '</div>' +
            '<div class="model-credit"><span class="credit-ratio">' + escapeHtml(t('detail.creditMultiplier', ratio)) + '</span></div>' +
            '<div class="model-info">' + escapeHtml(m.description || '') + '</div>' +
            '</div>';
        }).join('') || '<p class="empty-state">' + escapeHtml(t('detail.noModels')) + '</p>';
      } else {
        c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + ': ' + escapeHtml(d.error || '') + '</p>';
        toast(t('detail.loadFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + '</p>';
      toast(t('detail.loadFailed'), 'error');
    }
  }
  async function generateMachineId() {
    try {
      const res = await api('/generate-machine-id');
      const d = await res.json();
      if (d.machineId) $('machineIdInput').value = d.machineId;
    } catch (e) {
      toast(t('detail.generateFailed'), 'error');
    }
  }
  async function putAccount(id, body, successMsg) {
    try {
      const res = await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify(body) });
      const d = await res.json();
      if (d.success) {
        toast(successMsg, 'success');
        loadAccounts();
      } else {
        toast(t('detail.saveFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      toast(t('detail.saveFailed'), 'error');
    }
  }
  async function saveMachineId(id) {
    const m = $('machineIdInput').value.trim();
    if (m && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(m) && !/^[0-9a-f]{32}$/i.test(m)) {
      toast(t('detail.machineIdError'), 'warning'); return;
    }
    await putAccount(id, { machineId: m }, t('detail.saved'));
  }
  async function saveWeight(id) {
    const weight = parseInt($('weightInput').value, 10) || 0;
    await putAccount(id, { weight }, t('detail.saved'));
  }
  async function saveOverage(id) {
    const allowOverage = $('allowOverageInput').checked;
    let overageWeight = parseInt($('overageWeightInput').value, 10) || 1;
    overageWeight = Math.max(1, Math.min(10, overageWeight));
    $('overageWeightInput').value = overageWeight;
    await putAccount(id, { allowOverage, overageWeight }, t('detail.saved'));
  }
  async function saveProxyURL(id) {
    const url = $('proxyURLInput').value.trim();
    if (url && !/^(socks5|socks5h|http|https):\/\//.test(url)) {
      toast(t('detail.proxyFormatError'), 'warning'); return;
    }
    await putAccount(id, { proxyURL: url }, t('detail.proxySaved'));
  }
  function openGroupSelectModal(id, currentGroups) {
    currentGroupAccountId = id;
    const body = $('groupSelectBody');
    if (!body) return;
    body.innerHTML =
      '<p class="modal-hint">' + escapeHtml(t('groupSelect.hint')) + '</p>' +
      renderGroupCheckboxes(allGroups || [], currentGroups || [], 'group-select-checkbox') +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" type="button" data-group-action="cancel">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-primary" type="button" data-group-action="save">' + escapeHtml(t('common.save')) + '</button>' +
      '</div>';
    openDialog('groupSelectModal');
  }
  function closeGroupSelectModal() {
    closeDialog('groupSelectModal');
    currentGroupAccountId = null;
  }
  async function saveGroupFromModal() {
    const id = currentGroupAccountId;
    if (!id) return;
    const groups = qsa('.group-select-checkbox:checked', $('groupSelectBody')).map(cb => cb.value);
    try {
      const res = await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ groups }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('detail.saveFailed'));
      toast(t('detail.saved'), 'success');
      closeGroupSelectModal();
      await loadAccounts();
      await showDetail(id);
    } catch (e) {
      toast((e && e.message) || t('detail.saveFailed'), 'error');
    }
  }
  async function toggleStandby(id, standby) {
    try {
      const res = await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ standby }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('detail.saveFailed'));
      toast(standby ? t('detail.standbySet') : t('detail.standbyRemoved'), 'success');
      closeDetailModal();
      loadAccounts();
      loadStats();
    } catch (e) {
      toast((e && e.message) || t('detail.saveFailed'), 'error');
    }
  }
  function closeDetailModal() { closeDialog('detailModal'); }

  // Test flow
  function getTestAccount(id) {
    return accountsData.find(a => a.id === id) || null;
  }
  function getTestModelValue() {
    const choice = $('testModelChoice');
    return (choice && choice.value.trim()) || 'claude-sonnet-4';
  }
  function renderTestLog() {
    const c = $('testModalLog');
    if (!c) return;
    if (!testLogs.length) {
      c.innerHTML = '<div class="test-log-empty">' + escapeHtml(t('accounts.testLog.empty')) + '</div>';
      return;
    }
    c.innerHTML = testLogs.map(log =>
      '<div class="test-log-line ' + escapeAttr(log.type || 'info') + '">' +
      '<span class="test-log-time">' + escapeHtml(log.time) + '</span>' +
      '<span class="test-log-message">' + escapeHtml(log.msg) + '</span>' +
      '</div>'
    ).join('');
    c.scrollTop = c.scrollHeight;
  }
  function addTestLog(msg, type) {
    const time = new Date().toLocaleTimeString();
    testLogs.push({ time, msg, type });
    if (testLogs.length > 100) testLogs.shift();
    renderTestLog();
  }
  function clearTestLog() {
    testLogs = [];
    renderTestLog();
  }
  function renderTestModal() {
    const body = $('testBody');
    if (!body) return;
    const acc = getTestAccount(testModalAccountId);
    const idAttr = escapeAttr(testModalAccountId);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : testModalAccountId;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    const statusText = testModalLoadingModels
      ? t('accounts.testModelsLoading')
      : testModalModelError
        ? t('accounts.testModelsFallback')
        : t('accounts.testModelsReady', testModalModels.length);
    const modelField = testModalLoadingModels
      ? '<div class="test-model-loading">' + escapeHtml(t('accounts.testModelsLoading')) + '</div>'
      : testModalModels.length
        ? '<select id="testModelChoice">' +
        testModalModels.map(m => '<option value="' + escapeAttr(m) + '">' + escapeHtml(m) + '</option>').join('') +
        '</select>'
        : '<input type="text" id="testModelChoice" placeholder="claude-sonnet-4" value="claude-sonnet-4" />';

    body.innerHTML =
      '<div class="test-modal-account">' +
      '<div class="test-modal-account-main">' +
      '<div class="test-modal-email">' + escapeHtml(email) + '</div>' +
      '<div class="test-modal-meta">' +
      '<span>' + escapeHtml(formatAuthMethod(acc && (acc.provider || acc.authMethod))) + '</span>' +
      '<span>' + escapeHtml(proxy) + '</span>' +
      '</div>' +
      '</div>' +
      '<span class="test-modal-status">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      '<div class="test-modal-grid">' +
      '<div class="form-group test-model-field">' +
      '<label for="testModelChoice">' + escapeHtml(t('accounts.selectModel')) + '</label>' +
      modelField +
      '</div>' +
      '<div class="test-log-card">' +
      '<div class="test-log-header">' +
      '<span class="test-log-title">' + escapeHtml(t('accounts.testLog.title')) + '</span>' +
      '<button class="btn btn-xs btn-outline test-log-clear" id="testLogClear" type="button">' + escapeHtml(t('accounts.testLog.clear')) + '</button>' +
      '</div>' +
      '<div class="test-log-content" id="testModalLog"></div>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="testModalCancelBtn" type="button">' + escapeHtml(t('common.close')) + '</button>' +
      '<button class="btn btn-primary" id="testRunBtn" data-id="' + idAttr + '" type="button" ' + (testModalLoadingModels ? 'disabled' : '') + '>' + escapeHtml(t('accounts.test')) + '</button>' +
      '</div>';

    if (!testModalLoadingModels) enhanceCustomSelects(body);
    renderTestLog();
  }
  async function testAccount(id) {
    testModalAccountId = id;
    testModalModels = [];
    testModalLoadingModels = true;
    testModalModelError = false;
    testModalRunning = false;
    testLogs = [];
    renderTestModal();
    openDialog('testModal');
    try {
      const res = await api('/accounts/' + id + '/models/cached');
      const d = await res.json();
      testModalModels = Array.isArray(d.models) ? d.models.slice().sort() : [];
    } catch (e) {
      testModalModelError = true;
    } finally {
      testModalLoadingModels = false;
      renderTestModal();
    }
  }
  function closeTestModal() {
    closeAllCustomSelects();
    closeDialog('testModal');
  }
  async function runTestAccount(id, model) {
    if (testModalRunning) return;
    testModalRunning = true;
    const modalBtn = $('testRunBtn');
    if (modalBtn) modalBtn.setAttribute('aria-busy', 'true');
    const acc = accountsData.find(a => a.id === id);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : id;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    addTestLog(t('accounts.testLog.start', email, model, proxy), 'info');
    try {
      const startTime = Date.now();
      const res = await api('/accounts/' + id + '/test', { method: 'POST', body: JSON.stringify({ model }) });
      const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
      const d = await res.json();
      if (d.success) {
        addTestLog(t('accounts.testLog.success', email, elapsed, d.reply), 'ok');
      } else {
        addTestLog(t('accounts.testLog.failed', email, elapsed, d.error || t('common.unknownError')), 'err');
      }
    } catch (e) {
      addTestLog(t('accounts.testLog.error', email, e.message), 'err');
    }
    testModalRunning = false;
    if (modalBtn) modalBtn.removeAttribute('aria-busy');
  }

  // Settings
  async function loadSettings() {
    const res = await api('/settings');
    const d = await res.json();
    $('requireApiKey').checked = d.requireApiKey;
    $('apiKeyInput').value = d.apiKey || '';
    $('allowOverUsage').checked = d.allowOverUsage || false;
    const lb = $('loadBalancingMode');
    if (lb) lb.value = d.loadBalancingMode || 'balanced';
    await loadApiKeys();
    await loadGroupPolicies();
    await loadModelAliases();
    await Promise.all([loadThinkingConfig(), loadEndpointConfig(), loadProxyConfig(), loadPromptFilter()]);
    refreshCustomSelects();
  }
  async function loadThinkingConfig() {
    const res = await api('/thinking');
    const d = await res.json();
    $('thinkingSuffix').value = d.suffix || '-thinking';
    $('openaiThinkingFormat').value = d.openaiFormat || 'reasoning_content';
    $('claudeThinkingFormat').value = d.claudeFormat || 'thinking';
  }
  async function saveThinkingConfig() {
    const res = await api('/thinking', {
      method: 'POST', body: JSON.stringify({
        suffix: $('thinkingSuffix').value || '-thinking',
        openaiFormat: $('openaiThinkingFormat').value,
        claudeFormat: $('claudeThinkingFormat').value
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.thinkingSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadEndpointConfig() {
    const res = await api('/endpoint');
    const d = await res.json();
    $('preferredEndpoint').value = d.preferredEndpoint || 'auto';
    $('endpointFallback').checked = d.endpointFallback !== false;
  }
  async function saveEndpointConfig() {
    const res = await api('/endpoint', {
      method: 'POST', body: JSON.stringify({
        preferredEndpoint: $('preferredEndpoint').value,
        endpointFallback: $('endpointFallback').checked
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.endpointSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadProxyConfig() {
    const res = await api('/proxy');
    const d = await res.json();
    const url = d.proxyURL || '';
    if (!url) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
      return;
    }
    try {
      const u = new URL(url);
      const scheme = u.protocol.replace(':', '');
      $('proxyType').value = scheme.startsWith('socks5') ? 'socks5' : 'http';
      $('proxyHost').value = u.hostname;
      $('proxyPort').value = u.port;
      $('proxyUsername').value = decodeURIComponent(u.username);
      $('proxyPassword').value = decodeURIComponent(u.password);
      $('proxyFields').classList.remove('hidden');
    } catch (e) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
    }
  }
  function onProxyTypeChange() {
    const type = $('proxyType').value;
    $('proxyFields').classList.toggle('hidden', type === 'none');
  }
  async function saveProxyConfig() {
    const type = $('proxyType').value;
    let url = '';
    if (type !== 'none') {
      const host = $('proxyHost').value.trim();
      const port = $('proxyPort').value.trim();
      if (!host || !port) { toast(t('settings.proxyHostRequired'), 'warning'); return; }
      const u = $('proxyUsername').value.trim();
      const p = $('proxyPassword').value.trim();
      const auth = u ? (p ? encodeURIComponent(u) + ':' + encodeURIComponent(p) + '@' : encodeURIComponent(u) + '@') : '';
      url = type + '://' + auth + host + ':' + port;
    }
    const res = await api('/proxy', { method: 'POST', body: JSON.stringify({ proxyURL: url }) });
    const d = await res.json();
    if (d.success) toast(t('settings.proxySaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function saveApiSettings() {
    try {
      const requireApiKey = $('requireApiKey').checked;
      const apiKeyInput = $('apiKeyInput');
      if (requireApiKey && !apiKeyInput.value.trim() && apiKeysData.length === 0) generateApiKey();
      const keys = cleanApiKeys();
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ requireApiKey, apiKey: apiKeyInput.value }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      const keysRes = await api('/apikeys', { method: 'POST', body: JSON.stringify({ keys }) });
      const keysData = await keysRes.json().catch(() => ({}));
      if (!keysRes.ok || keysData.error) throw new Error(keysData.error || t('common.saveFailed'));
      toast(t('detail.saved'), 'success');
      loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function saveOverUsageConfig() {
    const allowOverUsage = $('allowOverUsage').checked;
    const loadBalancingMode = $('loadBalancingMode') ? $('loadBalancingMode').value : 'balanced';
    await api('/settings', { method: 'POST', body: JSON.stringify({ allowOverUsage, loadBalancingMode }) });
    toast(t('settings.overUsageSaved'), 'success');
  }
  async function changePassword() {
    const np = $('newPassword').value;
    if (!np) return toast(t('settings.passwordRequired'), 'warning');
    try {
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ password: np }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      setActivePassword(np, localStorage.getItem('kiro_remember') === '1');
      toast(t('settings.passwordChanged'), 'success');
      $('newPassword').value = '';
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function resetStats() {
    const ok = await confirmAction(t('settings.confirmReset'), {
      title: t('settings.statistics'),
      confirmText: t('settings.resetStats'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/stats/reset', { method: 'POST' });
      if (!res.ok) throw new Error(t('common.failed'));
      loadStats();
      toastPrimary(t('settings.statsReset'));
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    }
  }
  async function resetAccountEvents() {
    const ok = await confirmAction(t('settings.confirmResetAccountEvents'), {
      title: t('settings.resetAccountEvents'),
      confirmText: t('settings.resetAccountEvents'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/account-events/reset', { method: 'POST' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toastPrimary(t('settings.accountEventsReset'));
      if (statsActive) loadStatsAccountEvents();
      if (observeActive) loadObserve();
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    }
  }
  async function migrateCredentials() {
    const ok = await confirmAction(t('settings.confirmMigrate'), {
      title: t('settings.configMigration'),
      confirmText: t('settings.migrateCredentials'),
      variant: 'primary'
    });
    if (!ok) return;
    try {
      const res = await api('/migrate-credentials', { method: 'POST' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toastPrimary(d.message || t('settings.migrationSuccess'));
      if (d.migrated > 0) {
        setTimeout(() => loadAccounts(), 500);
      }
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    }
  }
  function generateApiKey() {
    const chars = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
    let k = 'sk-';
    const cryptoApi = window.crypto || window.msCrypto;
    if (!cryptoApi || !cryptoApi.getRandomValues) {
      toast(t('common.failed'), 'error');
      return;
    }
    const bytes = new Uint8Array(32);
    const limit = Math.floor(256 / chars.length) * chars.length;
    while (k.length < 35) {
      cryptoApi.getRandomValues(bytes);
      for (const b of bytes) {
        if (b >= limit) continue;
        k += chars.charAt(b % chars.length);
        if (k.length >= 35) break;
      }
    }
    $('apiKeyInput').value = k;
  }
  async function refreshGroupOptions() {
    try {
      const res = await api('/groups');
      const d = await res.json();
      allGroups = Array.isArray(d.groups) ? d.groups : [];
    } catch (e) {
      allGroups = [];
    }
  }
  async function loadApiKeys() {
    try {
      const res = await api('/apikeys');
      const d = await res.json();
      apiKeysData = (d.keys || []).map(k => ({
        id: k.id || '',
        name: k.name || '',
        key: k.key || '',
        groups: Array.isArray(k.groups) ? k.groups : [],
        enabled: k.enabled !== false,
        rpm: k.rpm || 0,
        rpd: k.rpd || 0,
        maxTokens: k.maxTokens || 0,
        requestCount: k.requestCount || 0,
        totalTokens: k.totalTokens || 0,
        totalCredits: k.totalCredits || 0,
        lastUsed: k.lastUsed || 0,
        usedMinute: k.usedMinute || 0,
        usedDay: k.usedDay || 0
      }));
    } catch (e) {
      apiKeysData = [];
    }
    await refreshGroupOptions();
    renderApiKeyTable();
  }
  function renderApiKeyTable() {
    const container = $('apiKeyTable');
    if (!container) return;
    if (!apiKeysData.length) {
      container.innerHTML = '<p class="empty-state compact">—</p>';
      return;
    }
    const header = '<div class="settings-grid settings-grid-apikey settings-grid-head">' +
      '<div>' + escapeHtml(t('settings.apiKeyName')) + '</div>' +
      '<div>' + escapeHtml(t('settings.apiKeyValue')) + '</div>' +
      '<div>' + escapeHtml(t('settings.apiKeyGroups')) + '</div>' +
      '<div>' + escapeHtml(t('settings.apiKeyRPM')) + '</div>' +
      '<div>' + escapeHtml(t('settings.apiKeyRPD')) + '</div>' +
      '<div>' + escapeHtml(t('settings.apiKeyMaxTokens')) + '</div>' +
      '<div title="' + escapeAttr(t('settings.apiKeyUsageTip')) + '">' + escapeHtml(t('settings.apiKeyUsage')) + '</div>' +
      '<div></div>' +
      '</div>';
    container.innerHTML = header + apiKeysData.map((k, i) => {
      const groupsValue = (k.groups || []).join(', ');
      const isAll = !k.groups || k.groups.length === 0 || k.groups.includes('*');
      const usage = '<div class="settings-usage" title="' + escapeAttr(t('settings.apiKeyUsageTip')) + '">' +
        '<div>' + (k.usedMinute || 0) + ' / ' + (k.usedDay || 0) + '</div>' +
        '<div>' + formatNum(k.requestCount || 0) + ' · ' + formatNum(k.totalTokens || 0) + '</div>' +
        '</div>';
      return '<div class="settings-grid settings-grid-apikey settings-grid-row">' +
        '<input type="text" value="' + escapeAttr(k.name) + '" data-apikey-idx="' + i + '" data-apikey-field="name" placeholder="' + escapeAttr(t('settings.apiKeyName')) + '" />' +
        '<input type="text" value="' + escapeAttr(k.key) + '" data-apikey-idx="' + i + '" data-apikey-field="key" placeholder="sk-..." class="font-mono" />' +
        '<input type="text" value="' + escapeAttr(groupsValue) + '" data-apikey-idx="' + i + '" data-apikey-field="groupsText" list="groupSuggestSettings" placeholder="' + escapeAttr(t('settings.apiKeyAll')) + '"' + (isAll ? ' title="*"' : '') + ' />' +
        '<input type="number" min="0" value="' + (k.rpm || 0) + '" data-apikey-idx="' + i + '" data-apikey-field="rpm" />' +
        '<input type="number" min="0" value="' + (k.rpd || 0) + '" data-apikey-idx="' + i + '" data-apikey-field="rpd" />' +
        '<input type="number" min="0" value="' + (k.maxTokens || 0) + '" data-apikey-idx="' + i + '" data-apikey-field="maxTokens" />' +
        usage +
        '<div class="settings-row-actions"><label class="switch"><input type="checkbox" ' + (k.enabled ? 'checked' : '') + ' data-apikey-idx="' + i + '" data-apikey-field="enabled" /><span class="slider"></span></label>' +
        '<button class="btn btn-icon btn-sm btn-danger" type="button" data-apikey-remove="' + i + '" aria-label="' + escapeAttr(t('common.remove')) + '">&times;</button></div>' +
        '</div>';
    }).join('') +
      '<datalist id="groupSuggestSettings">' + allGroups.map(g => '<option value="' + escapeAttr(g) + '"></option>').join('') + '</datalist>';
    refreshCustomSelects(container);
  }
  function addApiKeyRow() {
    const chars = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
    let key = 'sk-';
    const cryptoApi = window.crypto || window.msCrypto;
    if (cryptoApi && cryptoApi.getRandomValues) {
      const bytes = new Uint8Array(32);
      const limit = Math.floor(256 / chars.length) * chars.length;
      while (key.length < 35) {
        cryptoApi.getRandomValues(bytes);
        for (const b of bytes) {
          if (b >= limit) continue;
          key += chars.charAt(b % chars.length);
          if (key.length >= 35) break;
        }
      }
    } else {
      for (let i = 0; i < 32; i++) key += chars.charAt(Math.floor(Math.random() * chars.length));
    }
    apiKeysData.push({ id: '', name: '', key, groups: [], enabled: true, rpm: 0, rpd: 0, maxTokens: 0 });
    renderApiKeyTable();
  }
  function updateApiKeyField(i, field, value) {
    if (!apiKeysData[i]) return;
    if (field === 'groupsText') {
      apiKeysData[i].groups = String(value || '').split(',').map(s => s.trim()).filter(Boolean);
    } else if (field === 'rpm' || field === 'rpd' || field === 'maxTokens') {
      const n = parseInt(value, 10);
      apiKeysData[i][field] = Number.isFinite(n) && n > 0 ? n : 0;
    } else {
      apiKeysData[i][field] = value;
    }
  }
  function cleanApiKeys() {
    const seen = new Set();
    const cleaned = [];
    for (const k of apiKeysData || []) {
      const key = String(k.key || '').trim();
      if (!key) continue;
      if (seen.has(key)) throw new Error(t('settings.apiKeyDuplicate') + ': ' + key);
      seen.add(key);
      cleaned.push({
        id: k.id || '',
        name: String(k.name || '').trim(),
        key,
        groups: Array.isArray(k.groups) ? k.groups : [],
        enabled: k.enabled !== false,
        rpm: k.rpm || 0,
        rpd: k.rpd || 0,
        maxTokens: k.maxTokens || 0
      });
    }
    return cleaned;
  }
  async function loadGroupPolicies() {
    try {
      const res = await api('/group-policies');
      const d = await res.json();
      groupPoliciesData = (d.policies || []).map(p => ({
        name: p.name || '',
        allowedModels: Array.isArray(p.allowedModels) ? p.allowedModels : [],
        denyModels: Array.isArray(p.denyModels) ? p.denyModels : [],
        description: p.description || ''
      }));
    } catch (e) {
      groupPoliciesData = [];
    }
    await refreshGroupOptions();
    renderGroupPolicies();
  }
  function renderGroupPolicies() {
    const container = $('groupPolicyTable');
    if (!container) return;
    if (!groupPoliciesData.length) {
      container.innerHTML = '<p class="empty-state compact">—</p>';
      return;
    }
    const header = '<div class="settings-grid settings-grid-policy settings-grid-head">' +
      '<div>' + escapeHtml(t('settings.groupPolicyName')) + '</div>' +
      '<div>' + escapeHtml(t('settings.groupPolicyAllowed')) + '</div>' +
      '<div>' + escapeHtml(t('settings.groupPolicyDeny')) + '</div>' +
      '<div></div>' +
      '</div>';
    container.innerHTML = header + groupPoliciesData.map((p, i) => {
      return '<div class="settings-grid settings-grid-policy settings-grid-row">' +
        '<input type="text" value="' + escapeAttr(p.name) + '" data-policy-idx="' + i + '" data-policy-field="name" list="groupSuggestPolicy" placeholder="default / pro / free" />' +
        '<input type="text" value="' + escapeAttr((p.allowedModels || []).join(', ')) + '" data-policy-idx="' + i + '" data-policy-field="allowedText" placeholder="' + escapeAttr(t('settings.groupPolicyAllowedPlaceholder')) + '" class="font-mono" />' +
        '<input type="text" value="' + escapeAttr((p.denyModels || []).join(', ')) + '" data-policy-idx="' + i + '" data-policy-field="denyText" placeholder="' + escapeAttr(t('settings.groupPolicyDenyPlaceholder')) + '" class="font-mono" />' +
        '<button class="btn btn-icon btn-sm btn-danger" type="button" data-policy-remove="' + i + '" aria-label="' + escapeAttr(t('common.remove')) + '">&times;</button>' +
        '</div>';
    }).join('') +
      '<datalist id="groupSuggestPolicy"><option value="default"></option>' + allGroups.map(g => '<option value="' + escapeAttr(g) + '"></option>').join('') + '</datalist>';
  }
  function addGroupPolicyRow() {
    groupPoliciesData.push({ name: '', allowedModels: [], denyModels: [], description: '' });
    renderGroupPolicies();
  }
  function updateGroupPolicyField(i, field, value) {
    if (!groupPoliciesData[i]) return;
    if (field === 'allowedText') {
      groupPoliciesData[i].allowedModels = String(value || '').split(',').map(s => s.trim()).filter(Boolean);
    } else if (field === 'denyText') {
      groupPoliciesData[i].denyModels = String(value || '').split(',').map(s => s.trim()).filter(Boolean);
    } else {
      groupPoliciesData[i][field] = value;
    }
  }
  async function saveGroupPolicies() {
    try {
      const seen = new Set();
      const policies = [];
      for (const p of groupPoliciesData || []) {
        const name = String(p.name || '').trim();
        if (!name) continue;
        const key = name.toLowerCase();
        if (seen.has(key)) throw new Error(t('settings.groupPolicyDuplicate') + ': ' + name);
        seen.add(key);
        policies.push({
          name,
          allowedModels: p.allowedModels || [],
          denyModels: p.denyModels || [],
          description: String(p.description || '').trim()
        });
      }
      const res = await api('/group-policies', { method: 'POST', body: JSON.stringify({ policies }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.error) throw new Error(d.error || t('common.saveFailed'));
      toast(t('settings.groupPolicySaved'), 'success');
      await loadGroupPolicies();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function loadModelAliases() {
    try {
      const res = await api('/model-aliases');
      const d = await res.json();
      modelAliasesData = (d.aliases || []).map(a => ({
        from: a.from || '',
        to: a.to || '',
        enabled: a.enabled !== false,
        note: a.note || '',
        keyIds: Array.isArray(a.keyIds) ? a.keyIds.slice() : []
      }));
    } catch (e) {
      modelAliasesData = [];
    }
    renderModelAliases();
  }
  function renderModelAliases() {
    const container = $('modelAliasTable');
    if (!container) return;
    if (!modelAliasesData.length) {
      container.innerHTML = '<p class="empty-state compact">—</p>';
      return;
    }
    const keyOptions = (apiKeysData || []).filter(k => k.id).map(k => ({
      id: k.id,
      label: (k.name || '(unnamed)') + ' / ' + String(k.id).slice(0, 6)
    }));
    const header = '<div class="settings-grid settings-grid-alias settings-grid-head">' +
      '<div>' + escapeHtml(t('settings.modelAliasFrom')) + '</div>' +
      '<div>' + escapeHtml(t('settings.modelAliasTo')) + '</div>' +
      '<div>' + escapeHtml(t('settings.modelAliasNote')) + '</div>' +
      '<div>' + escapeHtml(t('settings.modelAliasBindKeys')) + '</div>' +
      '<div>' + escapeHtml(t('settings.apiKeyEnabled')) + '</div>' +
      '<div></div>' +
      '</div>';
    container.innerHTML = header + modelAliasesData.map((a, i) => {
      const bound = new Set((a.keyIds || []).map(String));
      const bindCell = keyOptions.length
        ? '<div class="alias-key-list">' + keyOptions.map(o =>
          '<label><input type="checkbox" ' + (bound.has(o.id) ? 'checked' : '') + ' data-alias-idx="' + i + '" data-alias-key="' + escapeAttr(o.id) + '" />' +
          '<span>' + escapeHtml(o.label) + '</span></label>'
        ).join('') + '</div>'
        : '<div class="muted-text text-xs">' + escapeHtml(t('settings.modelAliasNoKeys')) + '</div>';
      return '<div class="settings-grid settings-grid-alias settings-grid-row">' +
        '<input type="text" value="' + escapeAttr(a.from) + '" data-alias-idx="' + i + '" data-alias-field="from" placeholder="gpt-4o" class="font-mono" />' +
        '<input type="text" value="' + escapeAttr(a.to) + '" data-alias-idx="' + i + '" data-alias-field="to" placeholder="claude-sonnet-4-6" class="font-mono" />' +
        '<input type="text" value="' + escapeAttr(a.note) + '" data-alias-idx="' + i + '" data-alias-field="note" placeholder="' + escapeAttr(t('settings.modelAliasNote')) + '" />' +
        bindCell +
        '<label class="switch"><input type="checkbox" ' + (a.enabled ? 'checked' : '') + ' data-alias-idx="' + i + '" data-alias-field="enabled" /><span class="slider"></span></label>' +
        '<button class="btn btn-icon btn-sm btn-danger" type="button" data-alias-remove="' + i + '" aria-label="' + escapeAttr(t('common.remove')) + '">&times;</button>' +
        '</div>';
    }).join('');
  }
  function addModelAliasRow() {
    modelAliasesData.push({ from: '', to: '', enabled: true, note: '', keyIds: [] });
    renderModelAliases();
  }
  function updateModelAliasField(i, field, value) {
    if (!modelAliasesData[i]) return;
    modelAliasesData[i][field] = value;
  }
  function toggleModelAliasKeyId(i, keyId, checked) {
    if (!modelAliasesData[i]) return;
    const cur = Array.isArray(modelAliasesData[i].keyIds) ? modelAliasesData[i].keyIds : [];
    const set = new Set(cur.map(String));
    if (checked) set.add(String(keyId));
    else set.delete(String(keyId));
    modelAliasesData[i].keyIds = Array.from(set);
  }
  async function saveModelAliases() {
    try {
      const validKeyIds = new Set((apiKeysData || []).map(k => k.id).filter(Boolean));
      const seen = new Set();
      const aliases = [];
      for (const a of modelAliasesData || []) {
        const from = String(a.from || '').trim();
        const to = String(a.to || '').trim();
        if (!from || !to) continue;
        const key = from.toLowerCase();
        if (seen.has(key)) throw new Error(t('settings.modelAliasDuplicate') + ': ' + from);
        seen.add(key);
        aliases.push({
          from,
          to,
          enabled: a.enabled !== false,
          note: String(a.note || '').trim(),
          keyIds: Array.from(new Set((a.keyIds || []).map(s => String(s || '').trim()).filter(s => s && validKeyIds.has(s))))
        });
      }
      const res = await api('/model-aliases', { method: 'POST', body: JSON.stringify({ aliases }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.error) throw new Error(d.error || t('common.saveFailed'));
      toast(t('settings.modelAliasSaved'), 'success');
      await loadModelAliases();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }

  // Prompt filter rules
  async function loadPromptFilter() {
    const res = await api('/prompt-filter');
    const d = await res.json();
    $('filterClaudeCode').checked = !!d.filterClaudeCode;
    $('filterEnvNoise').checked = !!d.filterEnvNoise;
    $('filterStripBoundaries').checked = !!d.filterStripBoundaries;
    promptRules = d.rules || [];
    renderPromptRules();
  }
  async function savePromptFilter() {
    const res = await api('/prompt-filter', {
      method: 'POST', body: JSON.stringify({
        filterClaudeCode: $('filterClaudeCode').checked,
        filterEnvNoise: $('filterEnvNoise').checked,
        filterStripBoundaries: $('filterStripBoundaries').checked,
        rules: promptRules
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.promptFilterSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  function renderPromptRules() {
    const c = $('promptFilterRules');
    if (!c) return;
    if (!promptRules.length) {
      c.innerHTML = '<small class="text-xs muted-text">' + escapeHtml(t('promptFilter.noRules')) + '</small>';
      return;
    }
    c.innerHTML = promptRules.map((r, i) => {
      const isContains = r.type === 'lines-containing';
      const typeLabel = isContains ? t('promptFilter.typeContains') : t('promptFilter.typeRegex');
      const matchPh = isContains ? t('promptFilter.matchPlaceholderContains') : t('promptFilter.matchPlaceholderRegex');
      const replaceRow = !isContains
        ? '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.replace')) + '</label>' +
        '<input value="' + escapeAttr(r.replace || '') + '" data-rule-idx="' + i + '" data-rule-field="replace" placeholder="' + escapeAttr(t('promptFilter.emptyRemove')) + '" />' +
        '</div>'
        : '';
      return '<div class="rule-card' + (r.enabled ? '' : ' disabled') + '">' +
        '<div class="rule-header">' +
        '<label class="switch"><input type="checkbox" ' + (r.enabled ? 'checked' : '') + ' data-rule-toggle="' + i + '" /><span class="slider"></span></label>' +
        '<div class="rule-meta">' +
        '<input class="rule-name-input" value="' + escapeAttr(r.name || '') + '" data-rule-idx="' + i + '" data-rule-field="name" placeholder="' + escapeAttr(t('promptFilter.unnamed')) + '" />' +
        '<span class="rule-type">' + escapeHtml(typeLabel) + '</span>' +
        '</div>' +
        '<button class="rule-remove" data-rule-remove="' + i + '" type="button" aria-label="' + escapeAttr(t('common.remove')) + '">&times;</button>' +
        '</div>' +
        '<div class="rule-body">' +
        '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.match')) + '</label>' +
        '<input value="' + escapeAttr(r.match || '') + '" data-rule-idx="' + i + '" data-rule-field="match" placeholder="' + escapeAttr(matchPh) + '" />' +
        '</div>' +
        replaceRow +
        '</div>' +
        '</div>';
    }).join('');
  }
  function addPromptRule(type) {
    promptRules.push({ id: 'rule-' + Date.now(), name: '', type, match: '', replace: '', enabled: true });
    renderPromptRules();
  }

  // Add-account modal templates
  var METHOD_ICONS = {
    builderid: 'fa-solid fa-id-card',
    iam: 'fa-solid fa-key',
    sso: 'fa-solid fa-shield-halved',
    local: 'fa-solid fa-folder-open',
    credentials: 'fa-solid fa-code',
    cookie: 'fa-solid fa-cookie-bite'
  };
  function methodCard(type, title, desc) {
    var icon = METHOD_ICONS[type] || 'fa-solid fa-circle-plus';
    return '<button type="button" class="method-card" data-method="' + escapeAttr(type) + '">' +
      '<span class="method-icon"><i class="' + icon + '" aria-hidden="true"></i></span>' +
      '<span class="method-body">' +
      '<span class="method-title">' + escapeHtml(title) + '</span>' +
      '<span class="method-desc">' + escapeHtml(desc) + '</span>' +
      '</span>' +
      '<span class="method-arrow" aria-hidden="true"><i class="fa-solid fa-chevron-right"></i></span>' +
      '</button>';
  }
  function showModal(type) {
    const modal = $('addModal');
    const title = $('modalTitle');
    const body = $('modalBody');
    if (type === 'add') modalAdd(title, body);
    else if (type === 'builderid') modalBuilderId(title, body);
    else if (type === 'iam') modalIam(title, body);
    else if (type === 'sso') modalSso(title, body);
    else if (type === 'local') modalLocal(title, body);
    else if (type === 'credentials') modalCredentials(title, body);
    else if (type === 'cookie') modalCookie(title, body);
    if (!modal.classList.contains('active')) openDialog('addModal');
    enhanceCustomSelects(body);
  }
  function closeModal() {
    closeDialog('addModal');
    iamSession = '';
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    builderIdSession = '';
  }
  function modalAdd(title, body) {
    title.textContent = t('modal.addAccount');
    body.innerHTML =
      '<div class="method-list">' +
      methodCard('builderid', t('modal.builderIdTitle'), t('modal.builderIdDesc')) +
      methodCard('iam', t('modal.iamTitle'), t('modal.iamDesc')) +
      methodCard('sso', t('modal.ssoTitle'), t('modal.ssoDesc')) +
      methodCard('local', t('modal.localTitle'), t('modal.localDesc')) +
      methodCard('credentials', t('modal.credentialsTitle'), t('modal.credentialsDesc')) +
      methodCard('cookie', t('modal.cookieTitle'), t('modal.cookieDesc')) +
      '</div>' +
      '<div class="modal-footer"><button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>';
  }
  function modalBuilderId(title, body) {
    title.textContent = t('modal.builderIdTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.builderIdDesc')) + '</p>' +
      '<div id="builderIdStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="builderIdRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startBuilderIdBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="builderIdStep2" class="hidden">' +
      '<div class="message message-info message-center"><p class="builder-code" id="builderIdUserCode"></p><p class="text-xs mt-2">' + escapeHtml(t('builderid.verifyCode')) + '</p></div>' +
      '<div class="form-group mt-4"><label>' + escapeHtml(t('builderid.verifyUrl')) + '</label>' +
      '<div class="endpoint"><span id="builderIdVerifyUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p id="builderIdStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="builderIdCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('startBuilderIdBtn').addEventListener('click', startBuilderIdLogin);
  }
  function modalIam(title, body) {
    title.textContent = t('modal.iamTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.iamDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.startUrl')) + '</label><input type="text" id="iamStartUrl" placeholder="https://xxx.awsapps.com/start" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="iamRegion" value="us-east-1" /></div>' +
      '<div id="iamStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.loginUrl')) + '</label>' +
      '<div class="endpoint"><span id="iamAuthUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p class="text-sm mt-3 success-text">' + escapeHtml(t('iam.completeLogin')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.callbackUrl')) + '</label><input type="text" id="iamCallback" placeholder="http://127.0.0.1:xxx/?code=..." /></div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="iamBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>';
    $('iamBtn').addEventListener('click', startIamSso);
  }
  function modalSso(title, body) {
    title.textContent = t('modal.ssoTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<b>' + escapeHtml(t('sso.howToGet')) + '</b>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('sso.step1')) + ' <code class="code-inline">view.awsapps.com/start</code></li>' +
      '<li>' + escapeHtml(t('sso.step2')) + '</li>' +
      '<li>' + escapeHtml(t('sso.step3')) + ' <code class="code-inline">x-amz-sso_authn</code></li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('sso.tokenLabel')) + ' <small>' + escapeHtml(t('sso.tokenHint')) + '</small></label>' +
      '<textarea id="ssoToken" placeholder="' + escapeAttr(t('sso.tokenPlaceholder')) + '"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="ssoRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importSsoBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importSsoBtn').addEventListener('click', importSsoToken);
  }

  function modalLocal(title, body) {
    title.textContent = t('modal.localTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.localDesc')) + '</p>' +
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('local.fileLocation')) + '</b></p>' +
      '<p>' + escapeHtml(t('local.windows')) + ': <code class="code-inline">%USERPROFILE%\\.aws\\sso\\cache\\</code></p>' +
      '<p>' + escapeHtml(t('local.macosLinux')) + ': <code class="code-inline">~/.aws/sso/cache/</code></p>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('local.loginChannel')) + '</label>' +
      '<select id="localProvider">' +
      '<option value="BuilderId">' + escapeHtml(t('local.providerBuilderId')) + '</option>' +
      '<option value="Enterprise">' + escapeHtml(t('local.providerEnterprise')) + '</option>' +
      '<option value="Google">' + escapeHtml(t('local.providerGoogle')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('local.providerGithub')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group">' +
      '<label>' + escapeHtml(t('local.tokenFile')) + ' <small>' + escapeHtml(t('local.tokenRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localTokenJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localTokenFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div id="localClientGroup" class="form-group">' +
      '<label>' + escapeHtml(t('local.clientFile')) + ' <small>' + escapeHtml(t('local.clientRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localClientJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localClientFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importLocalBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('localProvider').addEventListener('change', updateLocalFields);
    $('localTokenFile').addEventListener('change', e => loadLocalFile(e.target, 'localTokenJson'));
    $('localClientFile').addEventListener('change', e => loadLocalFile(e.target, 'localClientJson'));
    $('importLocalBtn').addEventListener('click', importLocalKiro);
  }
  function modalCredentials(title, body) {
    title.textContent = t('modal.credentialsTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.credentialsDesc')) + '</p>' +
      '<p class="help-block">' + escapeHtml(t('credentials.batchHint')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('credentials.label')) + '</label>' +
      '<textarea id="credJson" class="font-mono" placeholder=\'[{"refreshToken":"xxx","provider":"BuilderID"}]\'></textarea>' +
      '</div>' +
      '<div class="form-group" style="margin-top:12px">' +
      '<label style="display:flex;align-items:center;cursor:pointer;user-select:none">' +
      '<input type="checkbox" id="importStandbyCheckbox" style="margin-right:8px">' +
      '<span>' + escapeHtml(t('import.standbyMode')) + '</span>' +
      '<span style="margin-left:8px;font-size:12px;color:#64748b;font-weight:normal">' + escapeHtml(t('import.standbyHint')) + '</span>' +
      '</label>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCredBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCredBtn').addEventListener('click', importCredentials);
  }
  function modalCookie(title, body) {
    title.textContent = t('modal.cookieTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('cookie.howToGet')) + '</b></p>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('cookie.step1')) + ' <a href="' + escapeAttr(t('cookie.link')) + '" target="_blank">' + escapeHtml(t('cookie.link')) + '</a></li>' +
      '<li>' + escapeHtml(t('cookie.step2')) + '</li>' +
      '<li>' + escapeHtml(t('cookie.step3')) + '</li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.provider')) + '</label>' +
      '<select id="cookieProvider">' +
      '<option value="Google">' + escapeHtml(t('cookie.google')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('cookie.github')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.refreshToken')) + '</label>' +
      '<textarea id="cookieRefreshToken" class="font-mono" placeholder="' + escapeAttr(t('cookie.refreshTokenPlaceholder')) + '"></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCookieBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCookieBtn').addEventListener('click', importFromCookie);
  }
  function updateLocalFields() {
    const p = $('localProvider').value;
    $('localClientGroup').classList.toggle('hidden', p === 'Google' || p === 'Github');
  }
  function loadLocalFile(input, targetId) {
    const file = input.files[0];
    if (!file) return;
    const r = new FileReader();
    r.onload = e => { $(targetId).value = e.target.result; };
    r.readAsText(file);
  }

  // Import handlers
  async function importLocalKiro() {
    const provider = $('localProvider').value;
    const tokenJson = $('localTokenJson').value.trim();
    const clientJson = $('localClientJson').value.trim();
    const isSocial = provider === 'Google' || provider === 'Github';
    if (!tokenJson) return toastWarning(t('local.tokenMissing'));
    let tokenData, clientData;
    try { tokenData = JSON.parse(tokenJson); } catch { return toastWarning(t('local.tokenInvalid')); }
    if (!tokenData.refreshToken) return toastWarning(t('local.refreshTokenMissing'));
    if (!isSocial) {
      if (!clientJson) return toastWarning(t('local.clientMissing'));
      try { clientData = JSON.parse(clientJson); } catch { return toastWarning(t('local.clientInvalid')); }
      if (!clientData.clientId || !clientData.clientSecret) return toastWarning(t('local.clientSecretMissing'));
    }
    const authMethod = clientData ? 'idc' : 'social';
    const payload = {
      refreshToken: tokenData.refreshToken,
      accessToken: tokenData.accessToken || '',
      clientId: clientData?.clientId || '',
      clientSecret: clientData?.clientSecret || '',
      authMethod, provider
    };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('local.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importCredentials() {
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const json = JSON.parse($('credJson').value.trim());
      let items;
      if (json.accounts && Array.isArray(json.accounts)) {
        items = json.accounts.map(a => {
          const c = a.credentials || {};
          return {
            refreshToken: c.refreshToken || a.refreshToken,
            clientId: c.clientId || a.clientId,
            clientSecret: c.clientSecret || a.clientSecret,
            region: c.region || a.region,
            authMethod: c.authMethod || a.authMethod,
            provider: c.provider || a.provider || a.idp
          };
        });
      } else {
        items = Array.isArray(json) ? json : [json];
      }
      let ok = 0, fail = 0, newIds = [];
      const standbyMode = $('importStandbyCheckbox')?.checked || false;
      for (const item of items) {
        if (!item.refreshToken) { fail++; continue; }
        let authMethod = item.authMethod || '';
        if (item.clientId && item.clientSecret) authMethod = 'idc';
        else if (!authMethod || authMethod === 'social') authMethod = 'social';
        else authMethod = authMethod.toLowerCase() === 'idc' ? 'idc' : 'social';
        let provider = item.provider || '';
        if (!provider && authMethod === 'social') provider = 'Google';
        if (!provider && authMethod === 'idc') provider = 'BuilderId';
        const payload = {
          refreshToken: item.refreshToken,
          accessToken: item.accessToken || '',
          clientId: item.clientId || '',
          clientSecret: item.clientSecret || '',
          authMethod, provider,
          region: item.region || 'us-east-1',
          standby: standbyMode
        };
        try {
          const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
          const d = await res.json();
          if (d.success) { ok++; if (d.account?.id) newIds.push(d.account.id); }
          else fail++;
        } catch { fail++; }
      }
      dismiss();
      closeModal(); loadAccounts(); loadStats();
      let msg = t('sso.importSuccess', ok);
      if (fail > 0) msg += t('sso.importPartial', fail);
      toastPrimary(msg, { duration: 5200 });
      newIds.forEach(autoRefreshNewAccount);
    } catch (e) {
      dismiss();
      toastWarning(t('credentials.jsonError'));
    }
  }
  async function importFromCookie() {
    const refreshToken = $('cookieRefreshToken').value.trim();
    if (!refreshToken) return toastWarning(t('cookie.refreshTokenMissing'));
    const provider = $('cookieProvider').value;
    const payload = { refreshToken, accessToken: '', clientId: '', clientSecret: '', authMethod: 'social', provider };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('cookie.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importSsoToken() {
    const res = await api('/auth/sso-token', {
      method: 'POST', body: JSON.stringify({
        bearerToken: $('ssoToken').value,
        region: $('ssoRegion').value
      })
    });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      const count = d.accounts?.length || 0;
      const errs = d.errors?.length || 0;
      let msg = t('sso.importSuccess', count);
      if (errs > 0) msg += t('sso.importPartial', errs);
      toastPrimary(msg, { duration: 5200 });
      if (d.accounts) d.accounts.forEach(a => autoRefreshNewAccount(a.id));
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function startBuilderIdLogin() {
    const region = $('builderIdRegion').value || 'us-east-1';
    const res = await api('/auth/builderid/start', { method: 'POST', body: JSON.stringify({ region }) });
    const d = await res.json();
    if (d.sessionId) {
      builderIdSession = d.sessionId;
      $('builderIdUserCode').textContent = d.userCode;
      $('builderIdVerifyUrl').textContent = d.verificationUri;
      $('builderIdStep1').classList.add('hidden');
      $('builderIdStep2').classList.remove('hidden');
      $('builderIdOpenBtn').addEventListener('click', () => window.open($('builderIdVerifyUrl').textContent, '_blank'));
      $('builderIdCopyBtn').addEventListener('click', async () => {
        await copyText($('builderIdVerifyUrl').textContent);
        toast(t('common.copied'), 'primary');
      });
      $('builderIdCancelBtn').addEventListener('click', cancelBuilderIdLogin);
      pollBuilderIdAuth(d.interval || 5);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  function pollBuilderIdAuth(interval) {
    builderIdPollTimer = setTimeout(async () => {
      const res = await api('/auth/builderid/poll', { method: 'POST', body: JSON.stringify({ sessionId: builderIdSession }) });
      const d = await res.json();
      if (d.completed) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else if (d.success && !d.completed) {
        $('builderIdStatus').textContent = t('builderid.waiting');
        pollBuilderIdAuth(d.interval || interval);
      } else {
        toastError(t('common.failed') + ': ' + (d.error || ''));
        cancelBuilderIdLogin();
      }
    }, interval * 1000);
  }
  function cancelBuilderIdLogin() {
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    builderIdSession = '';
    showModal('add');
  }
  async function startIamSso() {
    if (iamSession) {
      const res = await api('/auth/iam-sso/complete', {
        method: 'POST', body: JSON.stringify({
          sessionId: iamSession, callbackUrl: $('iamCallback').value
        })
      });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    } else {
      const res = await api('/auth/iam-sso/start', {
        method: 'POST', body: JSON.stringify({
          startUrl: $('iamStartUrl').value, region: $('iamRegion').value
        })
      });
      const d = await res.json();
      if (d.authorizeUrl) {
        iamSession = d.sessionId;
        $('iamAuthUrl').textContent = d.authorizeUrl;
        $('iamStep2').classList.remove('hidden');
        $('iamBtn').textContent = t('iam.complete');
        $('iamOpenBtn').addEventListener('click', () => window.open($('iamAuthUrl').textContent, '_blank'));
        $('iamCopyBtn').addEventListener('click', async () => {
          await copyText($('iamAuthUrl').textContent);
          toast(t('common.copied'), 'primary');
        });
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    }
  }
  async function autoRefreshNewAccount(id) {
    if (!id) return;
    try { await api('/accounts/' + id + '/refresh', { method: 'POST' }); } catch (e) { }
    loadAccounts();
  }

  // Export modal
  function showExportModal() {
    if (!accountsData.length) return toastWarning(t('accounts.empty'));
    exportSelectedIds = new Set(accountsData.map(a => a.id));
    renderExportModal();
    openDialog('exportModal');
  }
  function closeExportModal() { closeDialog('exportModal'); }
  function renderExportModal() {
    const body = $('exportBody');
    const all = exportSelectedIds.size === accountsData.length;
    body.innerHTML =
      '<div class="flex items-center justify-between mb-3">' +
      '<span class="text-sm muted-text">' + escapeHtml(t('export.selected', exportSelectedIds.size)) + '</span>' +
      '<button class="btn btn-sm btn-outline" id="exportToggleAllBtn" type="button">' + escapeHtml(all ? t('export.deselectAll') : t('export.selectAll')) + '</button>' +
      '</div>' +
      '<div class="export-list">' +
      accountsData.map(a => {
        const checked = exportSelectedIds.has(a.id);
        return '<label class="export-row' + (checked ? ' selected' : '') + '">' +
          '<input type="checkbox" ' + (checked ? 'checked' : '') + ' data-export-toggle="' + escapeAttr(a.id) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(getDisplayEmail(a.email, a.id)) + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + ' · ' + escapeHtml(formatSubscriptionLabel(a.subscriptionType)) + '</div>' +
          '</div>' +
          '</label>';
      }).join('') +
      '</div>' +
      '<div id="exportJsonPreview" class="hidden mb-3"><textarea id="exportJsonText" readonly class="font-mono"></textarea></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="exportCloseBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-outline" id="exportShowJsonBtn" type="button">' + escapeHtml(t('export.showJson')) + '</button>' +
      '<button class="btn btn-outline" id="exportCopyJsonBtn" type="button">' + escapeHtml(t('export.copyJson')) + '</button>' +
      '<button class="btn btn-primary" id="exportDownloadBtn" type="button">' + escapeHtml(t('export.downloadJson')) + '</button>' +
      '</div>';
    $('exportToggleAllBtn').addEventListener('click', () => {
      if (exportSelectedIds.size === accountsData.length) exportSelectedIds.clear();
      else exportSelectedIds = new Set(accountsData.map(a => a.id));
      renderExportModal();
    });
    $('exportCloseBtn').addEventListener('click', closeExportModal);
    $('exportShowJsonBtn').addEventListener('click', exportShowJson);
    $('exportCopyJsonBtn').addEventListener('click', exportCopyJson);
    $('exportDownloadBtn').addEventListener('click', exportDownloadJson);
    qsa('[data-export-toggle]', body).forEach(cb => cb.addEventListener('change', e => {
      const id = e.target.dataset.exportToggle;
      if (exportSelectedIds.has(id)) exportSelectedIds.delete(id);
      else exportSelectedIds.add(id);
      renderExportModal();
    }));
  }
  async function getExportData() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return null; }
    const res = await api('/export', { method: 'POST', body: JSON.stringify({ ids: Array.from(exportSelectedIds) }) });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toastError(t('common.failed') + ': ' + (err.error || t('common.unknownError')));
      return null;
    }
    return res.json();
  }
  async function exportShowJson() {
    const data = await getExportData();
    if (!data) return;
    $('exportJsonPreview').classList.remove('hidden');
    $('exportJsonText').value = JSON.stringify(data, null, 2);
  }
  async function exportCopyJson() {
    const data = await getExportData();
    if (!data) return;
    const filtered = (data.accounts || []).map(a => {
      const { clientId, clientSecret, accessToken, refreshToken } = a.credentials || {};
      return { clientId, clientSecret, accessToken, refreshToken };
    });
    await copyText(JSON.stringify(filtered, null, 2));
    toast(t('export.copied'), 'primary');
  }
  async function exportDownloadJson() {
    const data = await getExportData();
    if (!data) return;
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-accounts-' + new Date().toISOString().slice(0, 10) + '.json';
    a.click();
    URL.revokeObjectURL(url);
  }

  // Observability, backups, alerts, requests, stats
  function fmtNum(n) {
    if (n == null) return '0';
    const v = Number(n);
    if (!Number.isFinite(v)) return '0';
    if (v >= 1e9) return (v / 1e9).toFixed(2) + 'B';
    if (v >= 1e6) return (v / 1e6).toFixed(2) + 'M';
    if (v >= 1e3) return (v / 1e3).toFixed(1) + 'K';
    return Number.isInteger(v) ? String(v) : v.toFixed(2);
  }
  function fmtPct(p) {
    return ((Number(p) || 0) * 100).toFixed(2) + '%';
  }
  function fmtUptime(sec) {
    if (!sec || sec <= 0) return '-';
    const d = Math.floor(sec / 86400);
    const h = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const parts = [];
    if (d) parts.push(d + 'd');
    if (h) parts.push(h + 'h');
    parts.push(m + 'm');
    return parts.join(' ');
  }
  function fmtTime(ts) {
    if (!ts) return '-';
    const d = new Date(ts * 1000);
    if (Number.isNaN(d.getTime())) return '-';
    return d.toLocaleString();
  }
  async function observeFetch(path) {
    const res = await fetch(path, { headers: { 'X-Admin-Password': password } });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    return res.json();
  }
  async function loadObserve() {
    if (!observeActive) return;
    try {
      const [overview, heatmap, keys, mix, events, errs] = await Promise.all([
        observeFetch('/admin/api/observe/overview'),
        observeFetch('/admin/api/observe/account-heatmap?windowMin=60'),
        observeFetch('/admin/api/observe/keys'),
        observeFetch('/admin/api/observe/model-mix'),
        observeFetch('/admin/api/observe/account-events?limit=100'),
        observeFetch('/admin/api/observe/recent-errors?limit=100')
      ]);
      renderObserveOverview(overview);
      renderObserveHeatmap(heatmap);
      renderObserveKeys(keys);
      renderObserveModelMix(mix);
      renderObserveAccountEvents(events);
      renderObserveErrors(errs);
    } catch (e) { }
  }
  function renderObserveOverview(o) {
    const el = $('observeOverview');
    if (!el) return;
    o = o || {};
    const uptime = o.startedAt && o.nowUnix ? o.nowUnix - o.startedAt : 0;
    const uptimeEl = $('observeUptime');
    if (uptimeEl) uptimeEl.textContent = t('observe.startedAgo', fmtUptime(uptime));
    const cards = [
      ['observe.rpm1', fmtNum(o.rpm1 || 0)],
      ['observe.rpm5', fmtNum(o.rpm5Avg || 0)],
      ['observe.errRate5', fmtPct(o.errRate5 || 0)],
      ['observe.inTpm5', fmtNum(o.inTpm5 || 0)],
      ['observe.outTpm5', fmtNum(o.outTpm5 || 0)],
      ['observe.credits5', fmtNum(o.credits5 || 0)],
      ['observe.credits60', fmtNum(o.credits60 || 0)],
      ['observe.activeAccts', (o.activeAccts || 0) + ' / ' + (o.totalAccts || 0)],
      ['observe.totalModels', String(o.totalModels || 0)],
      ['observe.totalExhausted', String(o.totalExhausted || 0)],
      ['observe.todayExhausted', String(o.todayExhausted || 0)]
    ];
    el.innerHTML = cards.map(([k, v]) =>
      '<div class="stat-card"><div class="stat-value">' + escapeHtml(v) + '</div><div class="stat-label">' + escapeHtml(t(k)) + '</div></div>'
    ).join('');
  }
  function heatColor(reqs, failures, maxReqs) {
    if (reqs <= 0) return 'var(--muted)';
    const intensity = Math.min(1, reqs / Math.max(maxReqs, 1));
    if (failures > 0 && failures / reqs >= 0.3) return 'rgba(229,75,79,' + (0.25 + 0.6 * intensity).toFixed(2) + ')';
    return 'rgba(45,98,239,' + (0.2 + 0.7 * intensity).toFixed(2) + ')';
  }
  function renderObserveHeatmap(data) {
    const el = $('observeHeatmap');
    if (!el || !data) return;
    const rows = [{ id: '_global_', email: t('observe.global'), cells: data.global ? data.global.cells : [] }]
      .concat((data.accounts || []).map(a => ({ id: a.accountId, email: a.email || String(a.accountId || '').slice(0, 8), cells: a.cells || [] })));
    let maxReqs = 1;
    rows.forEach(r => (r.cells || []).forEach(c => { if (c.reqs > maxReqs) maxReqs = c.reqs; }));
    const cellW = 8, cellH = 14, labelW = 180, gap = 1;
    const windowMin = data.windowMin || 60;
    const w = labelW + windowMin * (cellW + gap);
    const h = rows.length * (cellH + gap) + 24;
    const parts = ['<svg width="' + w + '" height="' + h + '" xmlns="http://www.w3.org/2000/svg" style="font-family:inherit">'];
    for (let i = 0; i < windowMin; i += 10) {
      const x = labelW + (windowMin - 1 - i) * (cellW + gap);
      parts.push('<text x="' + x + '" y="12" font-size="10" fill="currentColor">-' + i + 'm</text>');
    }
    rows.forEach((r, ri) => {
      const y = 18 + ri * (cellH + gap);
      const label = (r.email || '').length > 24 ? r.email.slice(0, 22) + '...' : r.email;
      parts.push('<text x="0" y="' + (y + cellH - 3) + '" font-size="11" fill="currentColor">' + escapeHtml(label) + '</text>');
      (r.cells || []).forEach(c => {
        const x = labelW + (windowMin - 1 - c.offset) * (cellW + gap);
        const fill = heatColor(c.reqs, c.failures, maxReqs);
        const tip = r.email + ' · -' + c.offset + 'm · ' + c.reqs + ' req' + (c.failures > 0 ? ' (' + c.failures + ' fail)' : '') + (c.credits > 0 ? ' · ' + c.credits.toFixed(2) + ' cr' : '');
        parts.push('<rect x="' + x + '" y="' + y + '" width="' + cellW + '" height="' + cellH + '" fill="' + fill + '" stroke="var(--border)" stroke-width="0.5"><title>' + escapeHtml(tip) + '</title></rect>');
      });
    });
    parts.push('</svg>');
    el.innerHTML = parts.join('');
  }
  function tableHtml(heads, rows) {
    return '<table class="data-table"><thead><tr>' + heads.map(h => '<th>' + escapeHtml(h) + '</th>').join('') + '</tr></thead><tbody>' + rows.join('') + '</tbody></table>';
  }
  function renderObserveKeys(data) {
    const el = $('observeKeys');
    if (!el) return;
    const keys = (data && data.keys) || [];
    if (!keys.length) { el.innerHTML = '<div class="empty-state compact">' + escapeHtml(t('observe.empty')) + '</div>'; return; }
    el.innerHTML = tableHtml([
      t('observe.keyName'),
      t('observe.keyMinute') + ' (RPM)',
      t('observe.keyDay') + ' (RPD)',
      t('observe.keyTotalReq'),
      t('observe.keyTotalTokens'),
      t('observe.keyTotalCredits')
    ], keys.map(k => {
      const minTxt = k.usedMinute + (k.rpm > 0 ? ' / ' + k.rpm : '');
      const dayTxt = k.usedDay + (k.rpd > 0 ? ' / ' + k.rpd : '');
      return '<tr><td>' + escapeHtml(k.name || k.id) + (k.enabled ? '' : ' <span class="muted-text">(off)</span>') + '</td>' +
        '<td>' + escapeHtml(minTxt) + '</td><td>' + escapeHtml(dayTxt) + '</td>' +
        '<td>' + fmtNum(k.requestCount || 0) + '</td><td>' + fmtNum(k.totalTokens || 0) + '</td><td>' + fmtNum(k.totalCredits || 0) + '</td></tr>';
    }));
  }
  function renderObserveModelMix(data) {
    const el = $('observeModelMix');
    if (!el) return;
    const models = (data && data.models) || [];
    if (!models.length) { el.innerHTML = '<div class="empty-state compact">' + escapeHtml(t('observe.empty')) + '</div>'; return; }
    const total = models.reduce((s, m) => s + (m.credits || 0), 0) || 1;
    el.innerHTML = tableHtml([t('observe.modelName'), t('observe.modelReqs'), t('observe.modelTokens'), t('observe.modelCredits')], models.map(m => {
      const pct = ((m.credits || 0) / total) * 100;
      return '<tr><td class="font-mono">' + escapeHtml(m.model) + '</td><td>' + fmtNum(m.reqs || 0) + '</td><td>' + fmtNum(m.inTokens || 0) + ' / ' + fmtNum(m.outTokens || 0) + '</td>' +
        '<td><div class="meter-row"><span class="meter"><span style="width:' + pct.toFixed(1) + '%"></span></span><span>' + fmtNum(m.credits || 0) + ' (' + pct.toFixed(1) + '%)</span></div></td></tr>';
    }));
  }
  function renderObserveAccountEvents(data) {
    const el = $('observeAccountEvents');
    if (!el) return;
    const events = (data && data.events) || [];
    if (!events.length) { el.innerHTML = '<div class="empty-state compact">' + escapeHtml(t('observe.empty')) + '</div>'; return; }
    el.innerHTML = tableHtml([t('observe.eventTime'), t('observe.eventAccount'), t('observe.eventType'), t('observe.eventMessage')], events.map(e => {
      const acct = e.email ? getDisplayEmail(e.email, e.accountId) : String(e.accountId || '').slice(0, 8);
      const typeKey = 'observe.event' + String(e.eventType || '').charAt(0).toUpperCase() + String(e.eventType || '').slice(1);
      const typeLabel = t(typeKey) || e.eventType || '-';
      const typeBadge = '<span class="badge ' + (e.eventType === 'banned' ? 'badge-error' : 'badge-warning') + '">' + escapeHtml(typeLabel) + '</span>';
      return '<tr><td class="text-muted">' + escapeHtml(fmtTime(e.ts)) + '</td><td class="font-medium">' + escapeHtml(acct) + '</td><td>' + typeBadge + '</td><td>' + escapeHtml(e.reason || '-') + '</td></tr>';
    }));
  }
  function renderObserveErrors(data) {
    const el = $('observeRecentErrors');
    if (!el) return;
    const errs = (data && data.errors) || [];
    if (!errs.length) { el.innerHTML = '<div class="empty-state compact">' + escapeHtml(t('observe.empty')) + '</div>'; return; }
    el.innerHTML = tableHtml([t('observe.errTime'), t('observe.errAccount'), t('observe.errModel'), t('observe.errMessage')], errs.map(e => {
      const acct = e.email ? getDisplayEmail(e.email, e.accountId) : String(e.accountId || '').slice(0, 8);
      return '<tr><td>' + escapeHtml(fmtTime(e.ts)) + '</td><td>' + escapeHtml(acct) + '</td><td class="font-mono">' + escapeHtml(e.model || '-') + '</td><td class="danger-text">' + escapeHtml(e.message || '-') + '</td></tr>';
    }));
  }
  function startObserve() {
    observeActive = true;
    loadObserve();
    if (observeTimer) clearInterval(observeTimer);
    observeTimer = setInterval(loadObserve, 10000);
    ensureObserveSSE();
  }
  function stopObserve() {
    observeActive = false;
    if (observeTimer) { clearInterval(observeTimer); observeTimer = null; }
  }
  function ensureObserveSSE() {
    if (observeSSE) return;
    try {
      observeSSE = new EventSource('/admin/api/events?password=' + encodeURIComponent(password));
      observeSSE.addEventListener('observe_tick', () => {
        if (observeActive) loadObserve();
        if (requestsActive) loadRequests();
      });
      observeSSE.addEventListener('alert_fired', e => handleAlertFired(e.data));
      observeSSE.onerror = () => {
        if (observeSSE) { observeSSE.close(); observeSSE = null; }
        setTimeout(() => { if (observeActive || requestsActive) ensureObserveSSE(); }, 5000);
      };
    } catch (e) { }
  }
  function handleAlertFired(ruleId) {
    alertCount++;
    startAlertFlash();
    showBrowserNotification(ruleId);
    loadAlertsHistory();
  }
  function startAlertFlash() {
    if (alertFlashTimer) return;
    let flash = true;
    alertFlashTimer = setInterval(() => {
      document.title = flash ? '(' + alertCount + ') ' + originalTitle : originalTitle;
      flash = !flash;
    }, 1000);
  }
  function stopAlertFlash() {
    if (alertFlashTimer) clearInterval(alertFlashTimer);
    alertFlashTimer = null;
    alertCount = 0;
    document.title = t('app.title');
  }
  function showBrowserNotification(ruleId) {
    if (!('Notification' in window)) return;
    const show = () => new Notification('Kiro-Go Alert', { body: String(ruleId || ''), icon: '/admin/favicon.ico', tag: 'kiro-alert' });
    if (Notification.permission === 'granted') show();
    else if (Notification.permission !== 'denied') Notification.requestPermission().then(p => { if (p === 'granted') show(); });
  }
  async function loadBackups() {
    try {
      const res = await api('/backups?autoInclude=false');
      const data = await res.json();
      renderBackupsList(data.backups || []);
    } catch (e) { }
  }
  async function loadBackupSchedule() {
    try {
      const res = await api('/backups/schedule');
      const data = await res.json();
      const s = data.schedule || {};
      $('scheduleEnabled').checked = !!s.enabled;
      $('scheduleCadence').value = s.cadence || 'daily';
      $('scheduleKeep').value = s.keep || 50;
      refreshCustomSelects($('tabBackups'));
    } catch (e) { }
  }
  function renderBackupsList(backups) {
    const el = $('backupsList');
    if (!el) return;
    if (!backups.length) { el.innerHTML = '<div class="empty-state compact">' + escapeHtml(t('backups.empty')) + '</div>'; return; }
    const kindMap = { manual: 'backups.kindManual', auto: 'backups.kindAuto', scheduled: 'backups.kindScheduled', 'pre-restore': 'backups.kindPreRestore' };
    el.innerHTML = tableHtml([t('backups.id'), t('backups.createdAt'), t('backups.kind'), t('backups.size'), t('backups.accounts'), t('backups.note'), t('backups.actions')], backups.map(b => {
      const id = String(b.id || '');
      const sizeKB = ((b.size || 0) / 1024).toFixed(1) + ' KB';
      return '<tr><td class="font-mono">' + escapeHtml(id.slice(0, 16)) + '</td><td>' + escapeHtml(fmtTime(b.createdAt)) + '</td><td>' + escapeHtml(t(kindMap[b.kind] || 'backups.kindManual')) + '</td><td>' + escapeHtml(sizeKB) + '</td><td>' + (b.accountCnt || 0) + '</td><td>' + escapeHtml(b.note || '-') + '</td>' +
        '<td><div class="table-actions"><button class="btn btn-sm btn-outline" data-backup-action="download" data-id="' + escapeAttr(id) + '">' + escapeHtml(t('backups.download')) + '</button>' +
        '<button class="btn btn-sm btn-primary" data-backup-action="restore" data-id="' + escapeAttr(id) + '">' + escapeHtml(t('backups.restore')) + '</button>' +
        '<button class="btn btn-sm btn-danger" data-backup-action="delete" data-id="' + escapeAttr(id) + '">' + escapeHtml(t('backups.delete')) + '</button></div></td></tr>';
    }));
  }
  async function createBackupNow() {
    const note = window.prompt(t('backups.createNote'));
    if (note === null) return;
    try {
      const res = await api('/backups', { method: 'POST', body: JSON.stringify({ kind: 'manual', note }) });
      if (!res.ok) throw new Error(t('common.failed'));
      toast(t('backups.createSuccess'), 'success');
      loadBackups();
    } catch (e) { toastError((e && e.message) || t('common.failed')); }
  }
  function downloadBackup(id) {
    window.open('/admin/api/backups/' + encodeURIComponent(id) + '/download?password=' + encodeURIComponent(password), '_blank');
  }
  async function restoreBackup(id) {
    const ok = await confirmAction(t('backups.confirmRestore'), { title: t('backups.restore'), confirmText: t('backups.restore'), variant: 'danger' });
    if (!ok) return;
    try {
      const res = await api('/backups/' + encodeURIComponent(id) + '/restore', { method: 'POST' });
      if (!res.ok) throw new Error(t('common.failed'));
      toast(t('backups.restoreSuccess'), 'success');
      loadBackups(); loadAccounts(); loadSettings();
    } catch (e) { toastError((e && e.message) || t('common.failed')); }
  }
  async function deleteBackup(id) {
    const ok = await confirmAction(t('backups.confirmDelete'), { title: t('backups.delete'), confirmText: t('backups.delete'), variant: 'danger' });
    if (!ok) return;
    try {
      const res = await api('/backups/' + encodeURIComponent(id), { method: 'DELETE' });
      if (!res.ok) throw new Error(t('common.failed'));
      toast(t('backups.deleteSuccess'), 'success');
      loadBackups();
    } catch (e) { toastError((e && e.message) || t('common.failed')); }
  }
  async function saveBackupSchedule() {
    try {
      const keep = Math.max(1, parseInt($('scheduleKeep').value, 10) || 50);
      const res = await api('/backups/schedule', {
        method: 'POST',
        body: JSON.stringify({ enabled: $('scheduleEnabled').checked, cadence: $('scheduleCadence').value, keep })
      });
      if (!res.ok) throw new Error(t('common.failed'));
      toast(t('common.saved'), 'success');
    } catch (e) { toastError((e && e.message) || t('common.failed')); }
  }
  async function uploadBackup() {
    const input = $('backupFileInput');
    if (!input || !input.files.length) { toastWarning(t('backups.noFile')); return; }
    const formData = new FormData();
    formData.append('file', input.files[0]);
    try {
      const res = await fetch('/admin/api/backups/restore', { method: 'POST', headers: { 'X-Admin-Password': password }, body: formData });
      if (!res.ok) throw new Error(t('common.failed'));
      toast(t('backups.restoreSuccess'), 'success');
      input.value = '';
      loadBackups(); loadAccounts(); loadSettings();
    } catch (e) { toastError((e && e.message) || t('common.failed')); }
  }
  function startBackups() { loadBackups(); loadBackupSchedule(); }
  function stopBackups() { }
  async function loadAlerts() {
    try {
      const res = await api('/alerts');
      const data = await res.json();
      const rules = data.rules || [];
      const el = $('alertsList');
      if (!el) return;
      if (!rules.length) { el.innerHTML = '<div class="empty-state compact">' + escapeHtml(t('alerts.noRules')) + '</div>'; return; }
      el.innerHTML = rules.map(r => '<div class="list-row"><div><div class="font-semibold">' + escapeHtml(r.name) + '</div><div class="muted-text text-xs">' + escapeHtml((r.condition && r.condition.type) || '-') + ' · ' + escapeHtml((r.condition && r.condition.target) || 'global') + ' · ' + escapeHtml(t('alerts.threshold')) + ': ' + escapeHtml((r.condition && r.condition.threshold) || 0) + '</div></div>' +
        '<div class="table-actions"><label class="switch"><input type="checkbox" ' + (r.enabled ? 'checked' : '') + ' data-alert-action="toggle" data-id="' + escapeAttr(r.id) + '" /><span class="slider"></span></label>' +
        '<button class="btn btn-sm btn-outline" data-alert-action="edit" data-id="' + escapeAttr(r.id) + '">' + escapeHtml(t('alerts.edit')) + '</button>' +
        '<button class="btn btn-sm btn-outline" data-alert-action="test" data-id="' + escapeAttr(r.id) + '">' + escapeHtml(t('alerts.test')) + '</button>' +
        '<button class="btn btn-sm btn-danger" data-alert-action="delete" data-id="' + escapeAttr(r.id) + '">' + escapeHtml(t('common.delete')) + '</button></div></div>').join('');
    } catch (e) { }
  }
  async function loadAlertsHistory() {
    try {
      const res = await api('/alerts/history?limit=50');
      const data = await res.json();
      const history = data.history || [];
      const el = $('alertsHistory');
      if (!el) return;
      if (!history.length) { el.innerHTML = '<div class="empty-state compact">' + escapeHtml(t('alerts.noHistory')) + '</div>'; return; }
      el.innerHTML = history.map(h => '<div class="list-row"><div><div class="font-semibold">' + escapeHtml(h.rule_name || h.ruleName || '-') + '</div><div class="muted-text text-xs">' + escapeHtml(h.condition || '-') + '</div><div class="muted-text text-xs">Value: ' + Number(h.value || 0).toFixed(2) + ' · ' + escapeHtml(fmtTime(h.fired_at || h.firedAt)) + '</div></div></div>').join('');
    } catch (e) { }
  }
  async function openAlertModal(ruleId) {
    let rule = null;
    if (ruleId) {
      const res = await api('/alerts/' + encodeURIComponent(ruleId));
      const data = await res.json();
      rule = data.rule;
    }
    const conditionType = rule?.condition?.type || 'account_banned';
    const target = rule?.condition?.target || 'global';
    const actionType = rule?.actions?.[0]?.type || 'logsse';
    const webhookUrl = rule?.actions?.[0]?.config?.url || '';
    $('alertRuleModalTitle').textContent = ruleId ? t('alerts.edit') : t('alerts.create');
    $('alertRuleModalBody').innerHTML =
      '<div class="stack-form">' +
      '<div class="form-group"><label data-i18n="alerts.name"></label><input type="text" id="alertRuleName" value="' + escapeAttr(rule?.name || '') + '" data-i18n-placeholder="alerts.namePlaceholder" /></div>' +
      '<label class="flex items-center gap-2"><span class="switch"><input type="checkbox" id="alertRuleEnabled" ' + (rule?.enabled !== false ? 'checked' : '') + ' /><span class="slider"></span></span><span data-i18n="alerts.enabled"></span></label>' +
      '<div class="form-group"><label data-i18n="alerts.conditionType"></label><select id="alertConditionType"><option value="account_banned" ' + (conditionType === 'account_banned' ? 'selected' : '') + ' data-i18n="alerts.accountBanned"></option><option value="quota_exhausted" ' + (conditionType === 'quota_exhausted' ? 'selected' : '') + ' data-i18n="alerts.quotaExhausted"></option></select></div>' +
      '<div class="form-group"><label data-i18n="alerts.threshold"></label><input type="number" id="alertThreshold" value="' + escapeAttr(rule?.condition?.threshold || 1) + '" step="0.01" min="0" /></div>' +
      '<div class="form-group"><label data-i18n="alerts.window"></label><input type="number" id="alertWindow" value="' + escapeAttr(rule?.condition?.window || 5) + '" min="1" /></div>' +
      '<div class="form-group"><label data-i18n="alerts.target"></label><select id="alertTarget"><option value="global" ' + (target === 'global' ? 'selected' : '') + ' data-i18n="alerts.targetGlobal"></option><option value="account" ' + (target !== 'global' && target !== '' ? 'selected' : '') + ' data-i18n="alerts.targetAccount"></option></select><input type="text" id="alertTargetValue" value="' + escapeAttr(target !== 'global' ? target : '') + '" placeholder="account_id" class="' + (target === 'global' ? 'hidden' : '') + ' mt-2" /></div>' +
      '<div class="form-group"><label data-i18n="alerts.actions"></label><select id="alertActionType"><option value="logsse" ' + (actionType === 'logsse' ? 'selected' : '') + ' data-i18n="alerts.actionLogsse"></option><option value="webhook" ' + (actionType === 'webhook' ? 'selected' : '') + ' data-i18n="alerts.actionWebhook"></option></select><input type="text" id="alertWebhookUrl" value="' + escapeAttr(webhookUrl) + '" data-i18n-placeholder="alerts.webhookUrlPlaceholder" class="' + (actionType === 'webhook' ? '' : 'hidden') + ' mt-2" /></div>' +
      '<div class="form-group"><label data-i18n="alerts.cooldown"></label><input type="number" id="alertCooldown" value="' + escapeAttr(rule?.cooldown || 300) + '" min="60" /></div>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="alertCancelBtn" type="button" data-i18n="alerts.cancel"></button><button class="btn btn-primary" id="alertSubmitBtn" type="button" data-rule-id="' + escapeAttr(ruleId || '') + '" data-i18n="alerts.submit"></button></div>' +
      '</div>';
    applyTranslations();
    refreshCustomSelects($('alertRuleModalBody'));
    openDialog('alertRuleModal');
  }
  function closeAlertRuleModal() { closeDialog('alertRuleModal'); }
  async function submitAlertRule(ruleId) {
    const name = $('alertRuleName').value.trim();
    if (!name) { toastWarning(t('alerts.nameRequired')); return; }
    const actionType = $('alertActionType').value;
    const webhookUrl = $('alertWebhookUrl').value.trim();
    const actions = actionType === 'webhook' ? [{ type: 'webhook', config: { url: webhookUrl } }] : [{ type: 'logsse', config: {} }];
    if (actionType === 'webhook' && !webhookUrl) { toastWarning(t('alerts.webhookRequired')); return; }
    const target = $('alertTarget').value === 'global' ? 'global' : $('alertTargetValue').value.trim();
    const rule = {
      name,
      enabled: $('alertRuleEnabled').checked,
      condition: { type: $('alertConditionType').value, threshold: parseFloat($('alertThreshold').value) || 0, window: parseInt($('alertWindow').value, 10) || 5, target },
      actions,
      cooldown: parseInt($('alertCooldown').value, 10) || 300
    };
    try {
      const res = await api(ruleId ? '/alerts/' + encodeURIComponent(ruleId) : '/alerts', { method: ruleId ? 'PUT' : 'POST', body: JSON.stringify(rule) });
      if (!res.ok) throw new Error(t('common.saveFailed'));
      closeAlertRuleModal();
      loadAlerts();
      toast(t('common.saved'), 'success');
    } catch (e) { toastError((e && e.message) || t('common.saveFailed')); }
  }
  async function toggleAlert(id, enabled) {
    const res = await api('/alerts/' + encodeURIComponent(id));
    const data = await res.json();
    const rule = data.rule;
    rule.enabled = enabled;
    await api('/alerts/' + encodeURIComponent(id), { method: 'PUT', body: JSON.stringify(rule) });
    loadAlerts();
  }
  async function deleteAlert(id) {
    const ok = await confirmAction(t('alerts.confirmDelete'), { title: t('common.confirm'), confirmText: t('common.remove'), variant: 'danger' });
    if (!ok) return;
    await api('/alerts/' + encodeURIComponent(id), { method: 'DELETE' });
    loadAlerts();
  }
  async function testAlert(id) {
    try {
      const res = await api('/alerts/' + encodeURIComponent(id) + '/test', { method: 'POST' });
      const data = await res.json();
      toast('Triggered: ' + data.triggered + ' · Value: ' + Number(data.value || 0).toFixed(2), data.triggered ? 'warning' : 'primary');
    } catch (e) { toastError(t('common.failed')); }
  }
  function startAlerts() { loadAlerts(); loadAlertsHistory(); stopAlertFlash(); }
  function stopAlerts() { }
  async function loadRequests() {
    try {
      const res = await api('/observe/recent-requests?limit=200');
      const data = await res.json();
      let reqs = data.requests || [];
      const filterAccount = ($('filterAccount')?.value || '').toLowerCase().trim();
      const filterIP = ($('filterIP')?.value || '').toLowerCase().trim();
      if (filterAccount) reqs = reqs.filter(r => String(r.email || r.accountId || '').toLowerCase().includes(filterAccount));
      if (filterIP) reqs = reqs.filter(r => String(r.ip || '').toLowerCase().includes(filterIP));
      const el = $('requestsList');
      if (!el) return;
      if (!reqs.length) { el.innerHTML = '<div class="empty-state compact">' + escapeHtml(t('requests.empty')) + '</div>'; return; }
      el.innerHTML = tableHtml([t('requests.time'), t('requests.account'), t('requests.model'), t('requests.tokens'), t('requests.credits'), t('requests.remaining'), t('requests.ip'), t('requests.status')], reqs.map(r => {
        const account = r.email || String(r.accountId || '').slice(0, 8);
        return '<tr><td>' + escapeHtml(fmtTime(r.ts)) + '</td><td>' + escapeHtml(account) + '</td><td class="font-mono">' + escapeHtml(r.model || '-') + '</td><td>' + ((r.inTokens || 0) + (r.outTokens || 0)) + ' (' + (r.inTokens || 0) + '/' + (r.outTokens || 0) + ')</td><td>' + Number(r.credits || 0).toFixed(2) + '</td><td>' + Number(r.remainingCredits || 0).toFixed(2) + '</td><td>' + escapeHtml(r.ip || '-') + '</td><td><span class="badge ' + (r.success ? 'badge-success' : 'badge-error') + '">' + escapeHtml(r.success ? t('requests.success') : t('requests.failed')) + '</span></td></tr>';
      }));
    } catch (e) { }
  }
  function startRequests() {
    requestsActive = true;
    ensureObserveSSE();
    loadRequests();
  }
  function stopRequests() { requestsActive = false; }
  async function loadStatsAccountEvents() {
    if (!statsActive) return;
    try {
      const events = await observeFetch('/admin/api/observe/account-events?limit=100');
      renderStatsAccountEvents(events);
    } catch (e) { }
  }
  function renderStatsAccountEvents(data) {
    const el = $('statsAccountEvents');
    if (!el) return;
    const events = (data && data.events) || [];
    if (!events.length) { el.innerHTML = '<div class="empty-state compact">' + escapeHtml(t('observe.empty')) + '</div>'; return; }
    el.innerHTML = tableHtml([t('observe.eventTime'), t('observe.eventAccount'), t('observe.eventType'), t('observe.eventMessage')], events.map(e => {
      const acct = e.email ? getDisplayEmail(e.email, e.accountId) : String(e.accountId || '').slice(0, 8);
      const typeKey = 'observe.event' + String(e.eventType || '').charAt(0).toUpperCase() + String(e.eventType || '').slice(1);
      const typeLabel = t(typeKey) || e.eventType || '-';
      const typeBadge = '<span class="badge ' + (e.eventType === 'banned' ? 'badge-error' : 'badge-warning') + '">' + escapeHtml(typeLabel) + '</span>';
      return '<tr><td class="text-muted">' + escapeHtml(fmtTime(e.ts)) + '</td><td class="font-medium">' + escapeHtml(acct) + '</td><td>' + typeBadge + '</td><td>' + escapeHtml(e.reason || '-') + '</td></tr>';
    }));
  }
  function startStats() {
    statsActive = true;
    loadStatsAccountEvents();
    if (statsTimer) clearInterval(statsTimer);
    statsTimer = setInterval(loadStatsAccountEvents, 10000);
  }
  function stopStats() {
    statsActive = false;
    if (statsTimer) { clearInterval(statsTimer); statsTimer = null; }
  }

  // Version and update
  function renderVersionBadge() {
    const badge = $('versionBadge');
    if (badge && currentVersion) badge.textContent = currentVersion.replace(/^v/i, '');
  }
  async function loadVersion() {
    try {
      const res = await api('/version');
      const d = await res.json();
      currentVersion = d.version || '';
      renderVersionBadge();
    } catch (e) { }
  }
  function compareVersions(a, b) {
    const pa = a.split('.').map(Number);
    const pb = b.split('.').map(Number);
    for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
      const na = pa[i] || 0, nb = pb[i] || 0;
      if (na > nb) return 1;
      if (na < nb) return -1;
    }
    return 0;
  }
  function setUpdateButtonLoading(loading) {
    const btn = $('checkUpdateBtn');
    if (!btn) return;
    btn.disabled = loading;
    if (loading) btn.setAttribute('aria-busy', 'true');
    else btn.removeAttribute('aria-busy');
    const label = btn.querySelector('[data-update-label]');
    const icon = btn.querySelector('i');
    if (label) label.textContent = t(loading ? 'update.checking' : 'update.check');
    if (icon) icon.classList.toggle('fa-spin', loading);
  }
  async function checkUpdate(manual) {
    if (manual) setUpdateButtonLoading(true);
    try {
      if (!currentVersion) await loadVersion();
      const current = currentVersion.replace(/^v/i, '');
      if (!current) throw new Error('Current version missing');
      const res = await fetch('https://raw.githubusercontent.com/Quorinex/Kiro-Go/main/version.json?t=' + Date.now());
      if (!res.ok) throw new Error('Fetch failed');
      const d = await res.json();
      const latest = (d.version || '').replace(/^v/i, '');
      if (!latest) throw new Error('Latest version missing');
      if (latest && latest !== current && compareVersions(latest, current) > 0) {
        showUpdateToast('available', current, latest);
      } else if (manual) {
        showUpdateToast('current', current, latest || current);
      }
    } catch (e) {
      if (manual) showUpdateToast('error', '', '');
    } finally {
      if (manual) setUpdateButtonLoading(false);
    }
  }
  function showUpdateToast(status, current, latest) {
    if (status === 'available') {
      toast(t('update.availableToast') + (latest ? ': ' + latest : ''), 'warning', {
        icon: 'fa-solid fa-arrow-up',
        duration: 5200
      });
      return;
    }
    if (status === 'current') {
      toast(t('update.noUpdatesToast'), 'success', {
        icon: 'fa-solid fa-circle-check',
        duration: 3600
      });
      return;
    }
    toast(t('update.checkFailed'), 'error', {
      icon: 'fa-solid fa-triangle-exclamation',
      duration: 4200
    });
  }
  function showUpdateModal(version, url, changelog) {
    const current = currentVersion.replace(/^v/i, '');
    $('updateBody').innerHTML =
      '<div class="update-shell">' +
      '<div class="update-hero">' +
      '<div class="update-result-icon update-result-info"><i class="fa-solid fa-arrow-up"></i></div>' +
      '<div>' +
      '<h3 class="update-hero-title">' + escapeHtml(t('update.newVersion')) + '</h3>' +
      '<p class="update-hero-copy">' + escapeHtml(t('update.newVersionMessage')) + '</p>' +
      '</div>' +
      '</div>' +
      '<div class="update-version-grid">' +
      '<div class="update-version-card update-version-card-current"><p class="update-version-label">' + escapeHtml(t('update.current')) + '</p><p class="update-version-value update-version-value-current">' + escapeHtml(current) + '</p></div>' +
      '<div class="update-version-card update-version-card-latest"><p class="update-version-label">' + escapeHtml(t('update.latest')) + '</p><p class="update-version-value update-version-value-success">' + escapeHtml(version) + '</p></div>' +
      '</div>' +
      (changelog ? '<div class="update-notes"><p class="update-notes-title">' + escapeHtml(t('update.changelog')) + '</p><p class="update-notes-body">' + escapeHtml(changelog) + '</p></div>' : '') +
      '<div class="update-actions"><a href="' + escapeAttr(url) + '" target="_blank" rel="noopener" class="btn btn-primary">' + escapeHtml(t('update.goDownload')) + '</a></div>' +
      '</div>';
    openDialog('updateModal');
  }
  function showUpdateStatusModal(status, title, message, latest) {
    const current = currentVersion.replace(/^v/i, '');
    const isError = status === 'error';
    $('updateBody').innerHTML =
      '<div class="update-shell">' +
      '<div class="text-center mb-5">' +
      '<div class="update-result-icon update-status-icon update-result-' + (isError ? 'error' : 'success') + '">' +
      '<i class="fa-solid ' + (isError ? 'fa-triangle-exclamation' : 'fa-circle-check') + '"></i>' +
      '</div>' +
      '<p class="text-base font-semibold ' + (isError ? 'danger-text' : 'success-text') + '">' + escapeHtml(title) + '</p>' +
      '<p class="text-sm mt-2 muted-text">' + escapeHtml(message) + '</p>' +
      '</div>' +
      '<div class="update-version-grid">' +
      '<div class="update-version-card update-version-card-current"><p class="update-version-label">' + escapeHtml(t('update.current')) + '</p><p class="update-version-value update-version-value-current">' + escapeHtml(current || '-') + '</p></div>' +
      '<div class="update-version-card' + (!isError ? ' update-version-card-latest' : '') + '"><p class="update-version-label">' + escapeHtml(t('update.latest')) + '</p><p class="update-version-value' + (!isError ? ' update-version-value-success' : '') + '">' + escapeHtml(latest || '-') + '</p></div>' +
      '</div>' +
      '</div>';
    openDialog('updateModal');
  }
  function closeUpdateModal() { closeDialog('updateModal'); }

  // Tabs
  function switchTab(tab) {
    qsa('.tab').forEach(el => el.classList.toggle('active', el.dataset.tab === tab));
    qsa('.tab-content').forEach(c => c.classList.add('hidden'));
    const target = $('tab' + tab.charAt(0).toUpperCase() + tab.slice(1));
    if (target) target.classList.remove('hidden');
    if (tab === 'observe') { startObserve(); stopBackups(); stopAlerts(); stopRequests(); stopStats(); }
    else if (tab === 'backups') { startBackups(); stopObserve(); stopAlerts(); stopRequests(); stopStats(); }
    else if (tab === 'alerts') { startAlerts(); stopObserve(); stopBackups(); stopRequests(); stopStats(); }
    else if (tab === 'requests') { startRequests(); stopObserve(); stopBackups(); stopAlerts(); stopStats(); }
    else if (tab === 'stats') { startStats(); stopObserve(); stopBackups(); stopAlerts(); stopRequests(); }
    else { stopObserve(); stopBackups(); stopAlerts(); stopRequests(); stopStats(); }
    try { localStorage.setItem('kiro_current_tab', tab); } catch (e) { }
  }

  // Event wiring
  function bindLoginEvents() {
    $('loginBtn').addEventListener('click', login);
    $('pwdField').addEventListener('keypress', e => { if (e.key === 'Enter') login(); });

    const pwdToggle = $('pwdToggle');
    if (pwdToggle) {
      pwdToggle.addEventListener('click', () => {
        const f = $('pwdField');
        const willShow = f.type === 'password';
        f.type = willShow ? 'text' : 'password';
        pwdToggle.dataset.shown = String(willShow);
        pwdToggle.setAttribute('aria-label', willShow ? t('login.hidePassword') : t('login.showPassword'));
        pwdToggle.innerHTML = willShow
          ? '<i class="fa-solid fa-eye-slash"></i>'
          : '<i class="fa-solid fa-eye"></i>';
      });
    }
  }

  function bindShellEvents() {
    const checkUpdateBtn = $('checkUpdateBtn');
    if (checkUpdateBtn) checkUpdateBtn.addEventListener('click', () => checkUpdate(true));

    document.body.addEventListener('click', e => {
      if (!e.target.closest('.custom-select')) closeAllCustomSelects();
      const lb = e.target.closest('.lang-btn');
      if (lb) setLang(lb.dataset.lang);
      const lt = e.target.closest('.lang-toggle');
      if (lt) toggleLang();
    });
    window.addEventListener('resize', positionOpenCustomSelects);
    window.addEventListener('scroll', positionOpenCustomSelects, true);

    $('loginThemeToggle').addEventListener('click', toggleTheme);
    $('mainThemeToggle').addEventListener('click', toggleTheme);
    $('logoutBtn').addEventListener('click', logout);

    qsa('#tabBar button.tab[data-tab]').forEach(tab => tab.addEventListener('click', () => switchTab(tab.dataset.tab)));

    qsa('[data-copy]').forEach(btn => btn.addEventListener('click', async () => {
      const id = btn.dataset.copy;
      const target = $(id);
      if (!target) return;
      try {
        await copyText(target.dataset.rawValue || target.textContent);
        toast(t('common.copied'), 'primary');
      } catch (e) {
        toast(t('common.failed'), 'error');
      }
    }));
  }

  function bindAccountEvents() {
    $('privacyModeToggle').addEventListener('change', e => {
      privacyModeEnabled = e.target.checked;
      localStorage.setItem('privacyMode', privacyModeEnabled);
      renderAccounts();
    });

    $('refreshAccountsBtn').addEventListener('click', loadAccounts);
    $('exportBtn').addEventListener('click', showExportModal);
    $('importCredentialsBtn').addEventListener('click', () => showModal('credentials'));
    $('refreshAllModelsBtn').addEventListener('click', refreshAllModels);
    $('addAccountBtn').addEventListener('click', () => showModal('add'));

    $('selectAllCheckbox').addEventListener('change', e => toggleSelectAll(e.target.checked));
    qsa('[data-batch]').forEach(b => b.addEventListener('click', () => {
      const a = b.dataset.batch;
      if (a === 'refreshModels') batchRefreshModels();
      else if (a === 'setGroup') batchSetGroup();
      else if (a === 'clearGroup') batchClearGroup();
      else if (a === 'setStandby') batchSetStandby();
      else if (a === 'removeStandby') batchRemoveStandby();
      else if (a === 'delete') batchDelete();
      else batchAction(a);
    }));

    $('filterSearch').addEventListener('input', onFilterChange);
    $('filterStatusSelect').addEventListener('change', onFilterChange);
    $('filterGroupSelect').addEventListener('change', onFilterChange);
    $('sortSelect').addEventListener('change', onFilterChange);
    $('clearFiltersBtn').addEventListener('click', clearFilters);
    $('accountStatusTabs').addEventListener('click', e => {
      const tab = e.target.closest('.status-tab');
      if (tab) switchStatusTab(tab.dataset.status);
    });

    $('accountsList').addEventListener('click', e => {
      const cb = e.target.closest('.account-checkbox');
      if (cb) {
        toggleSelectAccount(cb.dataset.id);
        const card = cb.closest('.account-card');
        if (card) card.classList.toggle('selected', cb.checked);
        return;
      }
      const btn = e.target.closest('button[data-action]');
      if (!btn) return;
      const id = btn.dataset.id;
      const action = btn.dataset.action;
      if (action === 'refresh') refreshAccount(id, btn.closest('.account-card'));
      else if (action === 'detail') showDetail(id);
      else if (action === 'copyJSON') copyAccountJSON(id, btn);
      else if (action === 'toggle') toggleAccount(id, btn.dataset.enabled === 'true');
      else if (action === 'test') testAccount(id);
      else if (action === 'delete') deleteAccount(id);
    });
    $('accountsList').addEventListener('change', e => {
      const weight = e.target.closest('.quick-weight-select');
      if (weight) quickSetWeight(weight.dataset.id, weight.value);
    });
  }

  function bindSettingsEvents() {
    $('generateApiKeyBtn').addEventListener('click', generateApiKey);
    $('addApiKeyRowBtn').addEventListener('click', addApiKeyRow);
    $('saveApiSettingsBtn').addEventListener('click', saveApiSettings);
    $('addModelAliasBtn').addEventListener('click', addModelAliasRow);
    $('saveModelAliasesBtn').addEventListener('click', saveModelAliases);
    $('addGroupPolicyBtn').addEventListener('click', addGroupPolicyRow);
    $('saveGroupPoliciesBtn').addEventListener('click', saveGroupPolicies);
    $('saveOverUsageBtn').addEventListener('click', saveOverUsageConfig);
    $('saveThinkingBtn').addEventListener('click', saveThinkingConfig);
    $('saveEndpointBtn').addEventListener('click', saveEndpointConfig);
    $('changePasswordBtn').addEventListener('click', changePassword);
    $('proxyType').addEventListener('change', onProxyTypeChange);
    $('saveProxyBtn').addEventListener('click', saveProxyConfig);
    $('migrateCredentialsBtn').addEventListener('click', migrateCredentials);
    $('resetStatsBtn').addEventListener('click', resetStats);
    $('resetAccountEventsBtn').addEventListener('click', resetAccountEvents);

    $('apiKeyTable').addEventListener('input', e => {
      const idx = e.target.dataset.apikeyIdx;
      const field = e.target.dataset.apikeyField;
      if (idx != null && field) updateApiKeyField(parseInt(idx, 10), field, e.target.value);
    });
    $('apiKeyTable').addEventListener('change', e => {
      if (e.target.dataset.apikeyField === 'enabled') updateApiKeyField(parseInt(e.target.dataset.apikeyIdx, 10), 'enabled', e.target.checked);
    });
    $('apiKeyTable').addEventListener('click', e => {
      const rm = e.target.closest('[data-apikey-remove]');
      if (rm) { apiKeysData.splice(parseInt(rm.dataset.apikeyRemove, 10), 1); renderApiKeyTable(); renderModelAliases(); }
    });
    $('groupPolicyTable').addEventListener('input', e => {
      const idx = e.target.dataset.policyIdx;
      const field = e.target.dataset.policyField;
      if (idx != null && field) updateGroupPolicyField(parseInt(idx, 10), field, e.target.value);
    });
    $('groupPolicyTable').addEventListener('click', e => {
      const rm = e.target.closest('[data-policy-remove]');
      if (rm) { groupPoliciesData.splice(parseInt(rm.dataset.policyRemove, 10), 1); renderGroupPolicies(); }
    });
    $('modelAliasTable').addEventListener('input', e => {
      const idx = e.target.dataset.aliasIdx;
      const field = e.target.dataset.aliasField;
      if (idx != null && field) updateModelAliasField(parseInt(idx, 10), field, e.target.value);
    });
    $('modelAliasTable').addEventListener('change', e => {
      const idx = e.target.dataset.aliasIdx;
      if (idx == null) return;
      if (e.target.dataset.aliasField === 'enabled') updateModelAliasField(parseInt(idx, 10), 'enabled', e.target.checked);
      if (e.target.dataset.aliasKey) toggleModelAliasKeyId(parseInt(idx, 10), e.target.dataset.aliasKey, e.target.checked);
    });
    $('modelAliasTable').addEventListener('click', e => {
      const rm = e.target.closest('[data-alias-remove]');
      if (rm) { modelAliasesData.splice(parseInt(rm.dataset.aliasRemove, 10), 1); renderModelAliases(); }
    });
  }

  function bindPromptFilterEvents() {
    $('savePromptFilterBtn').addEventListener('click', savePromptFilter);
    $('addRuleRegexBtn').addEventListener('click', () => addPromptRule('regex'));
    $('addRuleContainsBtn').addEventListener('click', () => addPromptRule('lines-containing'));

    $('promptFilterRules').addEventListener('input', e => {
      const idx = e.target.dataset.ruleIdx;
      const field = e.target.dataset.ruleField;
      if (idx != null && field) promptRules[idx][field] = e.target.value;
    });
    $('promptFilterRules').addEventListener('change', e => {
      if (e.target.dataset.ruleToggle != null) {
        promptRules[e.target.dataset.ruleToggle].enabled = e.target.checked;
        renderPromptRules();
      }
    });
    $('promptFilterRules').addEventListener('click', e => {
      const rm = e.target.closest('[data-rule-remove]');
      if (rm) { promptRules.splice(parseInt(rm.dataset.ruleRemove, 10), 1); renderPromptRules(); }
    });
  }

  function bindModalEvents() {
    $('addModalClose').addEventListener('click', closeModal);
    $('detailModalClose').addEventListener('click', closeDetailModal);
    $('groupSelectModalClose').addEventListener('click', closeGroupSelectModal);
    $('batchGroupSelectModalClose').addEventListener('click', closeBatchGroupSelectModal);
    $('exportModalClose').addEventListener('click', closeExportModal);
    $('testModalClose').addEventListener('click', closeTestModal);
    $('updateModalClose').addEventListener('click', closeUpdateModal);
    [
      ['addModal', closeModal],
      ['detailModal', closeDetailModal],
      ['groupSelectModal', closeGroupSelectModal],
      ['batchGroupSelectModal', closeBatchGroupSelectModal],
      ['exportModal', closeExportModal],
      ['testModal', closeTestModal],
      ['updateModal', closeUpdateModal],
      ['alertRuleModal', closeAlertRuleModal],
      ['confirmModal', () => closeConfirm(false)],
    ].forEach(([id, fn]) => bindDialogBackdropClose(id, fn));

    $('modalBody').addEventListener('click', e => {
      const m = e.target.closest('[data-method]');
      if (m) { showModal(m.dataset.method); return; }
      const g = e.target.closest('[data-modal-goto]');
      if (g) { showModal(g.dataset.modalGoto); return; }
      if (e.target.dataset.closeAdd) closeModal();
    });
    $('groupSelectBody').addEventListener('click', e => {
      const action = e.target.closest('[data-group-action]');
      if (!action) return;
      if (action.dataset.groupAction === 'cancel') closeGroupSelectModal();
      else if (action.dataset.groupAction === 'save') saveGroupFromModal();
    });
    $('batchGroupSelectBody').addEventListener('click', e => {
      const action = e.target.closest('[data-batch-group-action]');
      if (!action) return;
      if (action.dataset.batchGroupAction === 'cancel') closeBatchGroupSelectModal();
      else if (action.dataset.batchGroupAction === 'save') saveBatchGroupFromModal();
    });
  }

  function bindDetailEvents() {
    $('detailBody').addEventListener('click', e => {
      if (e.target.id === 'generateMachineIdBtn') { generateMachineId(); return; }
      const tab = e.target.closest('[data-detail-tab]');
      if (tab) { switchDetailTab(tab.dataset.detailTab); return; }
      const b = e.target.closest('[data-detail-action]');
      if (!b) return;
      const id = b.dataset.id;
      const a = b.dataset.detailAction;
      if (a === 'saveMachineId') saveMachineId(id);
      else if (a === 'saveWeight') saveWeight(id);
      else if (a === 'saveOverage') saveOverage(id);
      else if (a === 'saveProxyURL') saveProxyURL(id);
      else if (a === 'selectGroups') {
        let groups = [];
        try { groups = JSON.parse(b.dataset.groups || '[]'); } catch (err) { groups = []; }
        openGroupSelectModal(id, groups);
      }
      else if (a === 'toggleStandby') toggleStandby(id, b.dataset.standby === 'true');
      else if (a === 'loadModels') loadModels(id);
      else if (a === 'refreshModels') refreshAccountModels(id);
    });
  }

  function bindTestEvents() {
    $('testBody').addEventListener('click', e => {
      if (e.target.id === 'testLogClear') { clearTestLog(); return; }
      if (e.target.id === 'testModalCancelBtn') { closeTestModal(); return; }
      const run = e.target.closest('#testRunBtn');
      if (run) runTestAccount(run.dataset.id, getTestModelValue());
    });
    $('testBody').addEventListener('keydown', e => {
      if (e.key !== 'Enter') return;
      if (!e.target.closest('#testModelChoice')) return;
      const run = $('testRunBtn');
      if (!run || run.disabled) return;
      e.preventDefault();
      runTestAccount(run.dataset.id, getTestModelValue());
    });
  }

  function bindEnhancedFeatureEvents() {
    $('createBackupBtn').addEventListener('click', createBackupNow);
    $('saveBackupScheduleBtn').addEventListener('click', saveBackupSchedule);
    $('uploadBackupBtn').addEventListener('click', uploadBackup);
    $('backupsList').addEventListener('click', e => {
      const btn = e.target.closest('[data-backup-action]');
      if (!btn) return;
      const id = btn.dataset.id;
      if (btn.dataset.backupAction === 'download') downloadBackup(id);
      else if (btn.dataset.backupAction === 'restore') restoreBackup(id);
      else if (btn.dataset.backupAction === 'delete') deleteBackup(id);
    });

    $('openAlertModalBtn').addEventListener('click', () => openAlertModal());
    $('alertsList').addEventListener('click', e => {
      const btn = e.target.closest('[data-alert-action]');
      if (!btn) return;
      const id = btn.dataset.id;
      const action = btn.dataset.alertAction;
      if (action === 'edit') openAlertModal(id);
      else if (action === 'test') testAlert(id);
      else if (action === 'delete') deleteAlert(id);
    });
    $('alertsList').addEventListener('change', e => {
      if (e.target.dataset.alertAction === 'toggle') toggleAlert(e.target.dataset.id, e.target.checked);
    });

    $('filterAccount').addEventListener('input', loadRequests);
    $('filterIP').addEventListener('input', loadRequests);

    $('alertRuleModalClose').addEventListener('click', closeAlertRuleModal);
    $('alertRuleModalBody').addEventListener('click', e => {
      if (e.target.id === 'alertCancelBtn') closeAlertRuleModal();
      const submit = e.target.closest('#alertSubmitBtn');
      if (submit) submitAlertRule(submit.dataset.ruleId);
    });
    $('alertRuleModalBody').addEventListener('change', e => {
      if (e.target.id === 'alertTarget') {
        const input = $('alertTargetValue');
        if (input) input.classList.toggle('hidden', e.target.value === 'global');
      }
      if (e.target.id === 'alertActionType') {
        const input = $('alertWebhookUrl');
        if (input) input.classList.toggle('hidden', e.target.value !== 'webhook');
      }
    });
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden) stopAlertFlash();
    });
  }

  function wireEvents() {
    bindLoginEvents();
    bindShellEvents();
    bindAccountEvents();
    bindSettingsEvents();
    bindPromptFilterEvents();
    bindModalEvents();
    bindDetailEvents();
    bindTestEvents();
    bindEnhancedFeatureEvents();
  }

  // Init
  async function init() {
    initTheme();
    await loadLocale(currentLang);
    if (currentLang !== 'zh') await loadLocale('zh');
    applyTranslations();
    initCustomSelectObserver();
    initPrivacyMode();
    initRememberMe();
    const yr = $('footerYear');
    if (yr) yr.textContent = new Date().getFullYear();
    wireEvents();
    if (password) {
      showMain();
      tryAutoLogin();
    } else {
      // 没有 password，移除 preauth 类，显示登录页
      document.documentElement.classList.remove('preauth');
    }
    setInterval(() => {
      if (!$('mainPage').classList.contains('hidden')) loadStats();
    }, 10000);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
