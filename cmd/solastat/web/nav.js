// nav.js — the top navigation bar shared by the dashboard and Settings,
// styled like solastat.com's: sticky, translucent black with blur, the app
// icon and the SolaStat wordmark on the left, links to the dashboard, the
// active plugins and Settings, and the language picker. The plugins sit
// behind one "Plugins" menu, however many there are: a panel of cards (icon,
// name, one line on what each does), grouped by category once there are
// several. On a phone the links fold into a menu button, the plugins listed
// in it as a section.
//
// It also adds the footer every page shares — copyright and app version —
// at the end of the page, so a plugin's page gets it like the dashboard.
//
// <script src="/nav.js" data-mode="minimal"> shows only the logo and the
// language picker (the first-run wizard, where the other pages aren't usable
// yet); window.solastatNav.setMinimal(false) brings the links back.
//
// Loaded right after i18n.js and before each page's own script, so its
// data-i18n labels are translated by the page's applyStaticI18n() and
// refreshed by setLang() like everything else.
//
// It also holds the theme: dark (the default, the pages' own colours) or
// light, picked with the sun/moon button and kept in this browser
// (solastat_theme). Light sets data-theme="light" on <html>; the light
// palette below overrides each page's colour variables, and each page has
// its own [data-theme="light"] rules for colours it doesn't take from them.
// Pages that draw in JS (charts) listen for "solastat:theme" to redraw.
(function () {
  const THEME_KEY = 'solastat_theme';
  const isLight = () => document.documentElement.dataset.theme === 'light';
  function applyTheme(theme) {
    if (theme === 'light') document.documentElement.dataset.theme = 'light';
    else delete document.documentElement.dataset.theme;
  }
  try { applyTheme(localStorage.getItem(THEME_KEY)); } catch (_) {}

  // Opened through the Orbit OS launcher the app lives under /solastat; the
  // launcher rewrites links in the HTML it serves, but not ones built here.
  const prefix = location.pathname.startsWith('/solastat') ? '/solastat' : '';
  const path = location.pathname.slice(prefix.length) || '/';
  const minimal = !!(document.currentScript && document.currentScript.dataset.mode === 'minimal');

  // Plugin ids are internal; each plugin reports its own name, description,
  // icon and category in /api/plugins (pluginapi.Info). KNOWN_NAMES only
  // covers a plugin named somewhere before that list has loaded — a license
  // key in Settings, say.
  const KNOWN_NAMES = { smartspot: 'Price Shift' };
  const CATEGORIES = ['energy', 'automation', 'analysis', 'tools'];
  // The menu groups by category from this many plugins up.
  const GROUP_FROM = 5;
  let plugins = [];
  const curLang = () => (typeof lang !== 'undefined' ? lang : 'en');
  const pick = (m) => (m && (m[curLang()] || m.en)) || '';
  const pluginName = (id) => {
    const p = plugins.find((x) => x.id === id);
    return pick(p && p.name) || KNOWN_NAMES[id] || id.charAt(0).toUpperCase() + id.slice(1);
  };
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);

  const LANGS = [
    ['en', '🇬🇧 English'], ['pt', '🇵🇹 Português'], ['es', '🇪🇸 Español'],
    ['de', '🇩🇪 Deutsch'], ['it', '🇮🇹 Italiano'], ['fr', '🇫🇷 Français'],
  ];

  // The buttons every page and plugin uses — primary (the sun gradient),
  // secondary (outlined) and danger — styled once, here. Put before each
  // page's own styles, so a page's variant (an icon, a width) still wins.
  const buttonCss = `
    button.primary, button.secondary, button.danger {
      display: inline-flex; align-items: center; justify-content: center; gap: 0.45rem;
      min-height: 2.25rem; padding: 0.5rem 1.15rem; border-radius: 0.6rem;
      font-family: inherit; font-size: 0.82rem; font-weight: 600; line-height: 1.2;
      letter-spacing: 0.005em; white-space: nowrap; cursor: pointer;
      transition: filter 0.15s, background-color 0.15s, border-color 0.15s, color 0.15s, box-shadow 0.15s, transform 0.08s;
    }
    button.primary {
      background: var(--accent-grad); border: 1px solid transparent; color: var(--accent-ink);
      box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.28), 0 4px 14px -6px rgba(245, 158, 11, 0.55);
    }
    button.primary:hover:not(:disabled) {
      filter: brightness(1.06);
      box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.3), 0 6px 18px -6px rgba(245, 158, 11, 0.7);
    }
    button.secondary {
      background: color-mix(in srgb, var(--text) 4%, transparent);
      border: 1px solid var(--border); color: var(--text);
    }
    button.secondary:hover:not(:disabled) {
      background: color-mix(in srgb, var(--text) 8%, transparent);
      border-color: color-mix(in srgb, var(--text) 22%, transparent);
    }
    button.danger {
      background: color-mix(in srgb, var(--danger) 6%, transparent);
      border: 1px solid color-mix(in srgb, var(--danger) 55%, transparent); color: var(--danger);
    }
    button.danger:hover:not(:disabled) {
      background: color-mix(in srgb, var(--danger) 14%, transparent); border-color: var(--danger);
    }
    button.primary:active:not(:disabled), button.secondary:active:not(:disabled),
    button.danger:active:not(:disabled) { transform: translateY(1px); }
    button.primary:focus-visible, button.secondary:focus-visible, button.danger:focus-visible {
      outline: 2px solid color-mix(in srgb, var(--accent) 70%, transparent); outline-offset: 2px;
    }
    button.primary:disabled, button.secondary:disabled, button.danger:disabled {
      opacity: 0.5; cursor: default; box-shadow: none;
    }`;

  // The layout every page shares, laid over each page's own styles: the
  // column's gutters, the page heading, cards, form fields, and on a phone
  // touch-sized fields and buttons that fill their row. Pages keep their
  // specific parts (charts, tiles, lists).
  const uiCss = `
    body {
      padding-left: max(1rem, env(safe-area-inset-left));
      padding-right: max(1rem, env(safe-area-inset-right));
      -webkit-font-smoothing: antialiased; -moz-osx-font-smoothing: grayscale;
      -webkit-tap-highlight-color: transparent;
    }
    .page-head { margin: 0.85rem 0.1rem 1.15rem; }
    .page-head h1 { font-size: 1.7rem; font-weight: 800; letter-spacing: -0.025em; line-height: 1.15; }
    .page-head p { color: var(--muted); font-size: 0.92rem; line-height: 1.45; margin-top: 0.3rem; }
    .card, .analytics-card, .flow-card {
      border-radius: 1.1rem; margin-bottom: 0.9rem;
      box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.035), 0 12px 32px -20px rgba(0, 0, 0, 0.8);
    }
    .card { padding: 1.15rem 1.2rem; }
    .card-title { font-size: 0.76rem; letter-spacing: 0.08em; }
    input[type="text"], input[type="number"], input[type="password"], input[type="time"], select {
      min-height: 2.75rem; border-radius: 0.65rem; padding: 0.55rem 0.8rem; font-size: 0.9rem;
      transition: border-color 0.15s, box-shadow 0.15s;
    }
    input[type="text"]:focus, input[type="number"]:focus, input[type="password"]:focus,
    input[type="time"]:focus, select:focus {
      box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 22%, transparent);
      border-color: color-mix(in srgb, var(--accent) 70%, var(--border));
    }
    .field select, .form-grid select { width: 100%; }
    @media (max-width: 560px) {
      .card { padding: 1rem 1.05rem; }
      .page-head h1 { font-size: 1.55rem; }
      /* 16px: below that, iOS zooms the page into a field it focuses. */
      input[type="text"], input[type="number"], input[type="password"], input[type="time"], select { font-size: 16px; }
      button.primary, button.secondary, button.danger { min-height: 2.75rem; }
      .btn-row > button.primary, .btn-row > button.secondary, .btn-row > button.danger { flex: 1 1 0; }
      .btn-row > .save-msg, .btn-row > .sm-status { flex-basis: 100%; }
      /* A form's odd last field takes the whole row, not half of it. */
      .form-grid > .field:last-child:nth-child(odd) { grid-column: 1 / -1; }
    }
    [data-theme="light"] .card, [data-theme="light"] .analytics-card, [data-theme="light"] .flow-card {
      box-shadow: 0 1px 2px rgba(0, 0, 0, 0.04), 0 10px 28px -18px rgba(15, 23, 42, 0.22);
    }`;

  // The light palette: the variables every page defines, on a light ground.
  // :root[data-theme] outranks each page's own :root, wherever it is.
  const lightCss = `
    :root[data-theme="light"] {
      color-scheme: light;
      --bg: #f3f4f7; --card: #ffffff; --card-2: #f4f4f5; --field: #fafafa;
      --border: #e4e4e7; --bar: #d4d4d8;
      --text: #18181b; --muted: #52525b; --faint: #a1a1aa;
      --accent: #d97706; --accent-ink: #1a1206;
      --solar: #d97706; --home: #2563eb; --grid: #6366f1; --battery: #059669;
      --ok: #059669; --warn: #d97706; --danger: #dc2626;
      --focus: #6366f1; --info: #6366f1;
      --charge: #16a34a; --discharge: #d97706;
    }
    :root[data-theme="light"] body {
      background:
        radial-gradient(90% 55% at 50% 0%, rgba(96, 165, 250, 0.10), transparent 70%),
        radial-gradient(80% 50% at 50% 100%, rgba(129, 140, 248, 0.10), transparent 70%),
        var(--bg);
      background-attachment: fixed;
    }
    [data-theme="light"] .app-nav { background: rgba(255, 255, 255, 0.78); border-bottom-color: rgba(0, 0, 0, 0.08); }
    [data-theme="light"] .app-nav-logo { color: #18181b; }
    [data-theme="light"] .app-nav-links a, [data-theme="light"] .app-nav-plug-btn { color: #27272a; }
    [data-theme="light"] .app-nav-links a.active, [data-theme="light"] .app-nav-plug-btn.active,
    [data-theme="light"] .app-nav-item.active .name { color: #b45309; }
    [data-theme="light"] .app-nav-plug-btn .sep, [data-theme="light"] .app-nav-group,
    [data-theme="light"] .app-nav-item .desc { color: #71717a; }
    [data-theme="light"] .app-nav-panel {
      background: rgba(255, 255, 255, 0.98); border-color: rgba(0, 0, 0, 0.08);
      box-shadow: 0 20px 50px rgba(0, 0, 0, 0.12);
    }
    [data-theme="light"] .app-nav-links a.app-nav-item:hover, [data-theme="light"] .app-nav-links a.app-nav-item.active { background: #f4f4f5; }
    [data-theme="light"] .app-nav-item .ico { background: rgba(0, 0, 0, 0.05); }
    [data-theme="light"] .app-nav-item .name { color: #18181b; }
    [data-theme="light"] .app-nav-lang-wrap, [data-theme="light"] .app-nav-theme { border-color: rgba(0, 0, 0, 0.15); }
    [data-theme="light"] .app-nav-lang-wrap:hover, [data-theme="light"] .app-nav-theme:hover { border-color: rgba(0, 0, 0, 0.3); }
    [data-theme="light"] .app-nav-lang { color: #3f3f46; border-color: rgba(0, 0, 0, 0.15); }
    [data-theme="light"] .app-nav-lang option { background: #fff; color: #18181b; }
    [data-theme="light"] .app-nav-toggle span { background: #3f3f46; }
    [data-theme="light"] .app-nav-sep { background: rgba(0, 0, 0, 0.1); }
    [data-theme="light"] .app-footer { border-top-color: rgba(0, 0, 0, 0.08); color: #71717a; }
    @media (max-width: 560px) {
      [data-theme="light"] .app-nav-links { background: rgba(255, 255, 255, 0.98); border-bottom-color: rgba(0, 0, 0, 0.08); }
      [data-theme="light"] .app-nav-links a { border-bottom-color: rgba(0, 0, 0, 0.06); }
      [data-theme="light"] .app-nav-panel, [data-theme="light"] .app-nav-panel.wide { background: none; box-shadow: none; }
    }`;

  const css = `
    html { overflow-x: clip; }
    body.has-app-nav { padding-top: 0; }
    .app-nav {
      position: sticky; top: 0; z-index: 300;
      /* Full-bleed bar while the page itself stays a centred column. */
      margin: 0 calc(50% - 50vw) 0.75rem;
      background: rgba(0, 0, 0, 0.72);
      backdrop-filter: saturate(180%) blur(20px);
      -webkit-backdrop-filter: saturate(180%) blur(20px);
      border-bottom: 1px solid rgba(255, 255, 255, 0.08);
    }
    .app-nav-inner {
      max-width: 42rem; margin: 0 auto; height: 52px;
      padding: 0 0.75rem; display: flex; align-items: center; gap: 1rem;
    }
    @media (min-width: 640px) { .app-nav-inner { padding: 0 1.5rem; } }
    .app-nav-logo {
      display: inline-flex; align-items: center; gap: 8px; flex: none;
      color: #fff; text-decoration: none; font-size: 17px;
    }
    .app-nav-logo img { border-radius: 7px; }
    .app-nav-wordmark { font-weight: 800; letter-spacing: -0.02em; }
    /* The edition (DIY by default) next to the wordmark (/api/version's edition). */
    .app-nav-edition {
      font-size: 0.6em; font-weight: 800; letter-spacing: 0.08em; line-height: 1;
      padding: 0.3em 0.45em 0.25em; margin-left: -2px; border-radius: 0.35em;
      color: #fbbf24; border: 1px solid rgba(251, 191, 36, 0.55);
    }
    [data-theme="light"] .app-nav-edition { color: #b45309; border-color: rgba(180, 83, 9, 0.5); }
    .app-nav-wordmark b {
      font-weight: inherit;
      background: linear-gradient(100deg, #fde047 0%, #f59e0b 45%, #f97316 100%);
      -webkit-background-clip: text; background-clip: text; color: transparent;
    }
    .app-nav-links { display: flex; gap: 1.25rem; margin-left: auto; align-items: center; }
    .app-nav-links a {
      color: #f5f5f7; opacity: 0.8; text-decoration: none; font-size: 13px;
      white-space: nowrap; transition: opacity 0.15s;
    }
    .app-nav-links a:hover { opacity: 1; }
    .app-nav-links a.active { opacity: 1; color: #fde047; }
    .app-nav-plugins { position: relative; }
    .app-nav-plugins[hidden] { display: none; }
    .app-nav-plug-btn {
      display: inline-flex; align-items: center; gap: 4px; padding: 0;
      background: none; border: 0; cursor: pointer; font: inherit; font-size: 13px;
      color: #f5f5f7; opacity: 0.8; white-space: nowrap;
    }
    .app-nav-plug-btn:hover, .app-nav-plugins.open .app-nav-plug-btn { opacity: 1; }
    .app-nav-plug-btn.active { opacity: 1; color: #fde047; }
    .app-nav-plug-btn .sep { color: #9ca3af; }
    .app-nav-plug-btn .chev { font-size: 9px; margin-left: 2px; transition: transform 0.15s; }
    .app-nav-plugins.open .chev { transform: rotate(180deg); }
    .app-nav-panel {
      display: none; position: absolute; top: calc(100% + 16px); right: -1rem; z-index: 310;
      width: 300px; padding: 8px;
      background: rgba(18, 18, 22, 0.97); border: 1px solid rgba(255, 255, 255, 0.08);
      border-radius: 14px; box-shadow: 0 20px 50px rgba(0, 0, 0, 0.55);
    }
    .app-nav-panel.wide { width: 460px; }
    .app-nav-plugins.open .app-nav-panel { display: block; }
    .app-nav-panel .grid { display: grid; grid-template-columns: 1fr; gap: 2px; }
    .app-nav-panel.wide .grid { grid-template-columns: 1fr 1fr; }
    .app-nav-group {
      font-size: 10.5px; letter-spacing: 0.08em; text-transform: uppercase; color: #9ca3af;
      padding: 8px 10px 4px;
    }
    .app-nav-links a.app-nav-item {
      display: flex; gap: 10px; align-items: flex-start; padding: 9px 10px; border-radius: 10px;
      opacity: 1; white-space: normal;
    }
    .app-nav-links a.app-nav-item:hover, .app-nav-links a.app-nav-item.active { background: #1a1a20; }
    .app-nav-item .ico {
      flex: none; width: 32px; height: 32px; border-radius: 9px; display: grid; place-items: center;
      font-size: 16px; background: rgba(255, 255, 255, 0.06);
    }
    .app-nav-item .name { font-size: 13.5px; font-weight: 600; color: #f5f5f7; }
    .app-nav-item.active .name { color: #fde047; }
    .app-nav-item .desc { font-size: 11.5px; color: #9ca3af; margin-top: 2px; line-height: 1.3; }
    .app-nav-phone-only, .app-nav-links a.app-nav-phone-only { display: none; }
    .app-nav-lang-wrap {
      position: relative; flex: none; width: 30px; height: 30px; border-radius: 999px;
      border: 1px solid rgba(255, 255, 255, 0.14); display: grid; place-items: center; font-size: 14px;
    }
    .app-nav-lang-wrap:hover { border-color: rgba(255, 255, 255, 0.3); }
    .app-nav-theme {
      flex: none; width: 30px; height: 30px; border-radius: 999px; padding: 0;
      border: 1px solid rgba(255, 255, 255, 0.14); background: none; cursor: pointer;
      display: grid; place-items: center; font-size: 14px; line-height: 1;
    }
    .app-nav-theme:hover { border-color: rgba(255, 255, 255, 0.3); }
    .app-nav-theme .label { display: none; }
    .app-nav-theme.bar, .app-nav-lang-wrap.bar { display: none; } /* the phone's, shown in its media query below */
    /* The native select sits invisible over the globe: one tap opens the
       usual language list. */
    .app-nav-lang-wrap .app-nav-lang { position: absolute; inset: 0; width: 100%; height: 100%; opacity: 0; }
    .app-nav-lang {
      background: transparent; color: #e5e7eb; border: 1px solid rgba(255, 255, 255, 0.14);
      border-radius: 999px; padding: 4px 8px; font: inherit; font-size: 12px; cursor: pointer;
      width: auto; flex: none; /* pages style select { width: 100% } for forms */
    }
    .app-nav-lang option { background: #111113; color: #f5f5f7; }
    .app-nav-toggle {
      display: none; margin-left: auto; width: 32px; height: 32px;
      background: none; border: 0; cursor: pointer; position: relative;
    }
    .app-nav-toggle span {
      position: absolute; left: 7px; right: 7px; height: 1.5px; background: #e5e7eb;
      transition: transform 0.25s, top 0.25s;
    }
    .app-nav-toggle span:first-child { top: 12px; }
    .app-nav-toggle span:last-child { top: 19px; }
    .app-nav.open .app-nav-toggle span:first-child { top: 16px; transform: rotate(45deg); }
    .app-nav.open .app-nav-toggle span:last-child { top: 16px; transform: rotate(-45deg); }
    @media (max-width: 560px) {
      .app-nav-toggle { display: block; }
      .app-nav-links {
        display: none; position: absolute; top: 52px; left: 0; right: 0;
        flex-direction: column; align-items: stretch; gap: 0; margin: 0;
        padding: 0.4rem 0.75rem 0.9rem;
        /* Near-opaque: the page behind mustn't show through the list. */
        background: rgba(8, 8, 10, 0.98);
        backdrop-filter: blur(20px); -webkit-backdrop-filter: blur(20px);
        border-bottom: 1px solid rgba(255, 255, 255, 0.08);
      }
      .app-nav.open .app-nav-links { display: flex; }
      .app-nav-links a {
        font-size: 16px; padding: 0.75rem 0.25rem;
        border-bottom: 1px solid rgba(255, 255, 255, 0.06);
      }
      .app-nav-lang { margin-top: 0.75rem; align-self: flex-start; font-size: 14px; padding: 6px 10px; }
      /* The plugins become a section of the menu: no button, no popover. */
      .app-nav-plug-btn { display: none; }
      .app-nav-panel, .app-nav-panel.wide {
        display: block; position: static; width: auto; padding: 0;
        background: none; border: 0; box-shadow: none;
      }
      .app-nav-panel.wide .grid { grid-template-columns: 1fr; }
      .app-nav-group { padding: 14px 0.25rem 4px; }
      .app-nav-phone-only { display: block; }
      .app-nav-links a.app-nav-phone-only { display: flex; }
      .app-nav-links a.app-nav-desk-only { display: none; }
      /* Settings isn't a plugin: a line sets it apart from their list. */
      .app-nav-sep { height: 1px; margin: 0.6rem 0.25rem; background: rgba(255, 255, 255, 0.1); }
      .app-nav-links a.app-nav-item { padding: 8px 0.5rem; margin: 0 -0.25rem; border-bottom: 0; }
      .app-nav-lang-wrap {
        width: auto; height: auto; margin-top: 0.75rem; align-self: flex-start;
        display: inline-flex; align-items: center; gap: 8px; border: 0; padding: 0;
      }
      .app-nav-lang-wrap .app-nav-lang { position: static; opacity: 1; margin: 0; }
      /* On a phone the language and theme buttons sit in the bar, left of
         the menu button, not in the menu. */
      .app-nav-links .app-nav-theme, .app-nav-links .app-nav-lang-wrap { display: none; }
      .app-nav-lang-wrap.bar {
        display: grid; margin: 0 0 0 auto; width: 30px; height: 30px; padding: 0; align-self: center;
        border: 1px solid rgba(255, 255, 255, 0.14);
      }
      .app-nav-lang-wrap.bar .app-nav-lang { position: absolute; inset: 0; width: 100%; height: 100%; opacity: 0; margin: 0; }
      .app-nav-theme.bar { display: grid; margin-left: 0.5rem; }
      .app-nav-toggle { margin-left: 0.5rem; }
    }
    /* Minimal: logo and language picker only, never folded into the menu. */
    .app-nav--minimal .app-nav-links a.app-nav-phone-only,
    .app-nav--minimal .app-nav-links > a,
    .app-nav--minimal .app-nav-plugins,
    .app-nav--minimal .app-nav-toggle { display: none; }
    .app-nav--minimal .app-nav-links {
      display: flex; position: static; flex-direction: row; margin-left: auto;
      padding: 0; background: none; border: 0;
      backdrop-filter: none; -webkit-backdrop-filter: none;
    }
    .app-nav--minimal .app-nav-lang { margin-top: 0; font-size: 12px; padding: 4px 8px; }
    .app-nav--minimal .app-nav-links { gap: 0.6rem; }
    .app-nav--minimal .app-nav-links .app-nav-theme { display: grid; }
    .app-nav--minimal .app-nav-links .app-nav-lang-wrap { display: inline-flex; align-self: center; }
    .app-nav--minimal .app-nav-theme.bar, .app-nav--minimal .app-nav-lang-wrap.bar { display: none; }
    .app-footer {
      /* Full-bleed like the bar, its content in the same column. */
      margin: 2rem calc(50% - 50vw) 0;
      border-top: 1px solid rgba(255, 255, 255, 0.08);
      font-size: 0.75rem; color: #9ca3af;
    }
    .app-footer-inner {
      max-width: 42rem; margin: 0 auto; padding: 0.75rem;
      display: flex; justify-content: space-between; align-items: center; gap: 1rem;
    }
    @media (min-width: 640px) { .app-footer-inner { padding: 0.75rem 1.5rem; } }
    .app-nav--minimal .app-nav-lang-wrap { width: auto; height: auto; border: 0; margin-top: 0; padding: 0; }
    .app-nav--minimal .app-nav-lang-wrap > span { display: none; }
    .app-nav--minimal .app-nav-lang-wrap .app-nav-lang {
      position: static; opacity: 1; border: 1px solid rgba(255, 255, 255, 0.14);
    }`;

  const style = document.createElement('style');
  style.textContent = css + lightCss;
  // The shared layout comes after each page's own <style>s, so it wins
  // where both style the same thing.
  const ui = document.createElement('style');
  ui.textContent = uiCss;
  document.head.appendChild(ui);
  document.head.appendChild(style);
  const buttons = document.createElement('style');
  buttons.textContent = buttonCss;
  document.head.insertBefore(buttons, document.head.firstChild);

  const isActive = (p) => (p === '/' ? path === '/' : path === p || path.startsWith(p + '/'));
  const link = (href, labelHtml, active) =>
    `<a class="app-nav-desk-only${active ? ' active' : ''}" href="${prefix}${href}">${labelHtml}</a>`;
  // In the phone's menu, the dashboard and Settings are cards like the
  // plugins': icon, name, one line on what's there.
  const pageCard = (href, icon, nameKey, name, descKey, desc, active) =>
    `<a class="app-nav-item app-nav-phone-only${active ? ' active' : ''}" href="${prefix}${href}">` +
    `<span class="ico" aria-hidden="true">${icon}</span>` +
    `<span><span class="name" data-i18n="${nameKey}">${name}</span><div class="desc" data-i18n="${descKey}">${desc}</div></span></a>`;

  const nav = document.createElement('nav');
  nav.className = minimal ? 'app-nav app-nav--minimal' : 'app-nav';
  nav.setAttribute('aria-label', 'SolaStat');
  nav.innerHTML = `
    <div class="app-nav-inner">
      <a class="app-nav-logo" href="${prefix}/" aria-label="SolaStat">
        <img src="${prefix}/favicon.svg" alt="" width="26" height="26">
        <span class="app-nav-wordmark"><b>Sola</b>Stat</span><span class="app-nav-edition" hidden></span>
      </a>
      <span class="app-nav-lang-wrap bar"><span aria-hidden="true">🌐</span>
        <select class="app-nav-lang" data-i18n-aria-label="lang_select_aria" aria-label="Language">
          ${LANGS.map(([code, label]) => `<option value="${code}">${label}</option>`).join('')}
        </select>
      </span>
      <button class="app-nav-theme bar" type="button"><span class="icon" aria-hidden="true"></span><span class="label"></span></button>
      <button class="app-nav-toggle" type="button" aria-expanded="false" data-i18n-aria-label="nav_menu_aria" aria-label="Menu">
        <span></span><span></span>
      </button>
      <div class="app-nav-links">
        ${link('/', '<span data-i18n="nav_dashboard">Dashboard</span>', isActive('/'))}
        ${pageCard('/', '📊', 'nav_dashboard', 'Dashboard', 'nav_dashboard_desc', 'Live energy and history', isActive('/'))}
        <div class="app-nav-plugins" hidden>
          <button class="app-nav-plug-btn" type="button" aria-haspopup="true" aria-expanded="false"></button>
          <div class="app-nav-panel"></div>
        </div>
        ${link('/settings', '<span data-i18n="nav_settings">Settings</span>', isActive('/settings'))}
        <div class="app-nav-sep app-nav-phone-only" role="separator"></div>
        ${pageCard('/settings', '⚙️', 'nav_settings', 'Settings', 'nav_settings_desc', 'Inverter, integrations, plugins and device', isActive('/settings'))}
        <button class="app-nav-theme" type="button"><span class="icon" aria-hidden="true"></span><span class="label"></span></button>
        <span class="app-nav-lang-wrap"><span aria-hidden="true">🌐</span>
          <select class="app-nav-lang" data-i18n-aria-label="lang_select_aria" aria-label="Language">
            ${LANGS.map(([code, label]) => `<option value="${code}">${label}</option>`).join('')}
          </select>
        </span>
      </div>
    </div>`;
  document.body.insertBefore(nav, document.body.firstChild);
  document.body.classList.add('has-app-nav');

  // confirmAlert asks before an action, as an iOS-style alert: a frosted
  // card over a blurred page, title and message centred, the two choices
  // side by side below a hairline. Resolves to true when confirmed; Cancel
  // and Esc resolve to false. A tap outside doesn't dismiss it (as on iOS),
  // so a stray tap never decides. destructive colours the confirm red.
  const alertCss = `
    .app-alert {
      position: fixed; inset: 0; z-index: 1000;
      display: flex; align-items: center; justify-content: center; padding: 1rem;
      background: rgba(0, 0, 0, 0.45);
      backdrop-filter: blur(6px); -webkit-backdrop-filter: blur(6px);
      animation: app-alert-fade 0.2s ease-out;
    }
    .app-alert-box {
      width: 270px; max-width: 100%; overflow: hidden; border-radius: 14px;
      background: rgba(40, 40, 44, 0.82);
      backdrop-filter: saturate(180%) blur(24px); -webkit-backdrop-filter: saturate(180%) blur(24px);
      box-shadow: 0 18px 50px rgba(0, 0, 0, 0.45);
      text-align: center; color: #fff;
      font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", system-ui, sans-serif;
      animation: app-alert-pop 0.28s cubic-bezier(0.2, 0.9, 0.3, 1.15);
    }
    .app-alert-text { padding: 1.15rem 1rem 1rem; }
    .app-alert-title { font-size: 1.06rem; font-weight: 600; line-height: 1.3; }
    .app-alert-msg { margin-top: 0.3rem; font-size: 0.81rem; line-height: 1.4; color: rgba(235, 235, 245, 0.75); }
    .app-alert-actions { display: flex; border-top: 0.5px solid rgba(255, 255, 255, 0.18); }
    .app-alert-actions button {
      flex: 1 1 0; min-height: 44px; padding: 0.6rem 0.5rem; margin: 0;
      background: none; border: 0; border-radius: 0; cursor: pointer;
      font: inherit; font-size: 1.06rem; color: #0a84ff;
    }
    .app-alert-actions button + button { border-left: 0.5px solid rgba(255, 255, 255, 0.18); }
    .app-alert-actions button:hover, .app-alert-actions button:focus-visible { background: rgba(255, 255, 255, 0.07); outline: none; }
    .app-alert-actions button:active { background: rgba(255, 255, 255, 0.14); }
    .app-alert-actions .cancel { font-weight: 600; }
    .app-alert-actions .destructive { color: #ff453a; }
    :root[data-theme="light"] .app-alert { background: rgba(0, 0, 0, 0.25); }
    :root[data-theme="light"] .app-alert-box { background: rgba(250, 250, 252, 0.86); color: #000; box-shadow: 0 18px 50px rgba(0, 0, 0, 0.2); }
    :root[data-theme="light"] .app-alert-msg { color: rgba(60, 60, 67, 0.8); }
    :root[data-theme="light"] .app-alert-actions { border-top-color: rgba(60, 60, 67, 0.25); }
    :root[data-theme="light"] .app-alert-actions button { color: #007aff; }
    :root[data-theme="light"] .app-alert-actions button + button { border-left-color: rgba(60, 60, 67, 0.25); }
    :root[data-theme="light"] .app-alert-actions button:hover,
    :root[data-theme="light"] .app-alert-actions button:focus-visible { background: rgba(0, 0, 0, 0.04); }
    :root[data-theme="light"] .app-alert-actions button:active { background: rgba(0, 0, 0, 0.09); }
    :root[data-theme="light"] .app-alert-actions .destructive { color: #ff3b30; }
    @keyframes app-alert-fade { from { opacity: 0; } }
    @keyframes app-alert-pop { from { opacity: 0; transform: scale(1.12); } }
    @media (prefers-reduced-motion: reduce) { .app-alert, .app-alert-box { animation: none; } }`;
  const alertStyle = document.createElement('style');
  alertStyle.textContent = alertCss;
  document.head.appendChild(alertStyle);

  function confirmAlert({ title, message = '', okLabel, cancelLabel, destructive = false }) {
    const tr = (k, fallback) => (typeof t === 'function' ? t(k) : fallback);
    const el = document.createElement('div');
    el.className = 'app-alert';
    el.innerHTML = `
      <div class="app-alert-box" role="alertdialog" aria-modal="true">
        <div class="app-alert-text"><div class="app-alert-title"></div><div class="app-alert-msg"></div></div>
        <div class="app-alert-actions"><button type="button" class="cancel"></button><button type="button" class="ok"></button></div>
      </div>`;
    const box = el.querySelector('.app-alert-box');
    const titleEl = el.querySelector('.app-alert-title');
    const msgEl = el.querySelector('.app-alert-msg');
    const cancelBtn = el.querySelector('.cancel');
    const okBtn = el.querySelector('.ok');
    titleEl.textContent = title;
    titleEl.id = 'app-alert-title';
    box.setAttribute('aria-labelledby', titleEl.id);
    if (message) {
      msgEl.textContent = message;
      msgEl.id = 'app-alert-msg';
      box.setAttribute('aria-describedby', msgEl.id);
    } else {
      msgEl.remove();
    }
    cancelBtn.textContent = cancelLabel || tr('btn_cancel', 'Cancel');
    okBtn.textContent = okLabel || 'OK';
    if (destructive) okBtn.classList.add('destructive');
    const before = document.activeElement;
    document.body.appendChild(el);
    cancelBtn.focus();
    return new Promise((resolve) => {
      const close = (answer) => {
        document.removeEventListener('keydown', onKey, true);
        el.remove();
        if (before && typeof before.focus === 'function') before.focus();
        resolve(answer);
      };
      // Esc cancels; Tab stays between the two buttons.
      const onKey = (e) => {
        if (e.key === 'Escape') { e.preventDefault(); close(false); }
        else if (e.key === 'Tab') {
          e.preventDefault();
          (document.activeElement === cancelBtn ? okBtn : cancelBtn).focus();
        }
      };
      document.addEventListener('keydown', onKey, true);
      cancelBtn.addEventListener('click', () => close(false));
      okBtn.addEventListener('click', () => close(true));
    });
  }

  window.solastatNav = {
    setMinimal(on) { nav.classList.toggle('app-nav--minimal', !!on); },
    // An iOS-style confirmation (see confirmAlert): resolves to true when
    // confirmed.
    confirm: confirmAlert,
    // Settings passes its fresh /api/plugins list here after activating or
    // removing a key, so the menu updates without waiting for the poll.
    setPlugins(list) { renderPlugins(list); },
    // A plugin's display name, by id — as it reports it, in the current
    // language (Settings names the plugins a license key unlocks).
    pluginName,
    // Whether the light theme is on, for pages that draw their own colours.
    isLight,
  };

  const toggle = nav.querySelector('.app-nav-toggle');
  toggle.addEventListener('click', (e) => {
    e.stopPropagation();
    const open = nav.classList.toggle('open');
    toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
  });
  document.addEventListener('click', (e) => {
    if (!nav.contains(e.target)) {
      nav.classList.remove('open');
      toggle.setAttribute('aria-expanded', 'false');
    }
    if (!pluginsEl.contains(e.target)) setPanel(false);
  });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') setPanel(false); });

  // Two pickers too: in the links, and in the phone's bar. CSS shows one.
  const langSels = nav.querySelectorAll('.app-nav-lang');
  if (typeof lang !== 'undefined') for (const sel of langSels) sel.value = lang;
  // The server keeps the language too, for the LCD: told on every pick, and
  // on load when this browser has one picked (a browser that never picked
  // must not undo another's pick). The server only writes a change.
  const postLang = (l) => fetch('/api/language', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ language: l }),
  }).catch(() => {});
  let pickedLang = null;
  try { pickedLang = localStorage.getItem('solastat_lang'); } catch (_) {}
  if (pickedLang) postLang(pickedLang);
  for (const sel of langSels) sel.addEventListener('change', (e) => {
    for (const other of langSels) other.value = e.target.value;
    if (typeof setLang === 'function') setLang(e.target.value);
    postLang(e.target.value);
    // Pages that render text in JS (charts, statuses) listen for this.
    document.dispatchEvent(new CustomEvent('solastat:lang', { detail: e.target.value }));
  });
  // Plugin names and descriptions come in every language: redraw in the new one.
  document.addEventListener('solastat:lang', () => renderPlugins(plugins));

  // The theme button shows what a tap switches to: the sun in the dark
  // theme, the moon in the light one.
  // Two of them: in the links (desktop, and the wizard's minimal bar) and in
  // the bar next to the menu button (phone); CSS shows one.
  const themeBtns = nav.querySelectorAll('.app-nav-theme');
  function renderThemeBtn() {
    const label = isLight() ? t('nav_theme_dark', 'Dark theme') : t('nav_theme_light', 'Light theme');
    for (const b of themeBtns) {
      b.querySelector('.icon').textContent = isLight() ? '🌙' : '☀️';
      b.querySelector('.label').textContent = label;
      b.setAttribute('aria-label', label);
      b.title = label;
    }
  }
  for (const b of themeBtns) b.addEventListener('click', (e) => {
    e.stopPropagation();
    const theme = isLight() ? 'dark' : 'light';
    applyTheme(theme);
    try { localStorage.setItem(THEME_KEY, theme); } catch (_) {}
    renderThemeBtn();
    document.dispatchEvent(new CustomEvent('solastat:theme', { detail: theme }));
  });
  document.addEventListener('solastat:lang', renderThemeBtn);

  // The Plugins menu: one card per licensed plugin — /api/plugins only
  // lists plugins that verified their own key, and a plugin's page is
  // always "/<id>". Hidden altogether with none.
  const pluginsEl = nav.querySelector('.app-nav-plugins');
  const plugBtn = nav.querySelector('.app-nav-plug-btn');
  const panel = nav.querySelector('.app-nav-panel');
  function setPanel(open) {
    pluginsEl.classList.toggle('open', open);
    plugBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
  }
  plugBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    setPanel(!pluginsEl.classList.contains('open'));
  });

  const t = (key, fallback) => (typeof I18N !== 'undefined' && I18N[curLang()] && I18N[curLang()][key]) || fallback;
  // A plugin's page: its own path, else "/<id>".
  const pagePath = (p) => (p.path && p.path.startsWith('/') ? p.path : '/' + p.id);
  function card(p) {
    const active = isActive(pagePath(p));
    const desc = pick(p.description);
    return `<a class="app-nav-item${active ? ' active' : ''}" href="${prefix}${esc(pagePath(p))}">` +
      `<span class="ico" aria-hidden="true">${esc(p.icon || '🧩')}</span>` +
      `<span><span class="name">${esc(pluginName(p.id))}</span>` +
      (desc ? `<div class="desc">${esc(desc)}</div>` : '') + `</span></a>`;
  }
  function renderPlugins(list) {
    plugins = list || [];
    pluginsEl.hidden = plugins.length === 0;
    const current = plugins.find((p) => isActive(pagePath(p)));
    plugBtn.classList.toggle('active', !!current);
    plugBtn.innerHTML = esc(t('nav_plugins', 'Plugins')) +
      (current ? ` <span class="sep">›</span> ${esc(pluginName(current.id))}` : '') +
      ' <span class="chev" aria-hidden="true">▼</span>';

    // By category once there are several, the known ones in a fixed order.
    const grouped = plugins.length >= GROUP_FROM;
    const order = (c) => { const i = CATEGORIES.indexOf(c); return i < 0 ? CATEGORIES.length : i; };
    const sorted = [...plugins].sort((a, b) => order(a.category) - order(b.category));
    let html = '';
    if (grouped) {
      for (const c of [...new Set(sorted.map((p) => p.category || ''))]) {
        const label = c ? t('nav_cat_' + c, c.charAt(0).toUpperCase() + c.slice(1)) : t('nav_cat_other', 'Other');
        html += `<div class="app-nav-group">${esc(label)}</div><div class="grid">` +
          sorted.filter((p) => (p.category || '') === c).map(card).join('') + '</div>';
      }
    } else {
      // On a phone the menu still needs a heading for the plugins' section.
      html = `<div class="app-nav-group app-nav-phone-only">${esc(t('nav_plugins', 'Plugins'))}</div>` +
        `<div class="grid">${sorted.map(card).join('')}</div>`;
    }
    panel.classList.toggle('wide', plugins.length > 4);
    panel.innerHTML = html;
  }
  function refreshPlugins() {
    fetch(`${location.origin}${prefix}/api/plugins`)
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => renderPlugins(d && d.plugins))
      .catch(() => {});
  }
  refreshPlugins();
  setInterval(() => { if (!document.hidden) refreshPlugins(); }, 30000);
  renderThemeBtn();

  // ── Footer ── appended once the page's own content is in place, so it
  // always ends the page; the version comes from the core.
  const footer = document.createElement('footer');
  footer.className = 'app-footer';
  footer.innerHTML = `
    <div class="app-footer-inner">
      <span data-i18n="footer_copyright">© 2026 SolaStat. All rights reserved.</span>
      <span class="app-footer-version"></span>
    </div>`;
  const placeFooter = () => {
    document.body.appendChild(footer);
    if (typeof applyStaticI18n === 'function') applyStaticI18n();
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', placeFooter);
  else placeFooter();
  // The edition (DIY by default) next to every logo on the page, the
  // kiosk's too: the last one known at once (it never changes on a device),
  // then the device's.
  const showEdition = (ed) => {
    if (!ed) return;
    const put = () => document.querySelectorAll('.app-nav-edition').forEach((el) => {
      el.textContent = ed;
      el.hidden = false;
    });
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', put);
    else put();
  };
  try { showEdition(localStorage.getItem('solastat-edition')); } catch (e) {}
  fetch(`${location.origin}${prefix}/api/version`)
    .then((r) => (r.ok ? r.json() : null))
    .then((d) => {
      if (!d) return;
      if (d.version) footer.querySelector('.app-footer-version').textContent = 'v' + d.version;
      if (d.edition) {
        showEdition(d.edition);
        try { localStorage.setItem('solastat-edition', d.edition); } catch (e) {}
      }
    })
    .catch(() => {});
})();
