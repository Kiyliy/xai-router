# Console design system

The admin console is plain HTML/CSS/JS with no build step. Every visual decision lives in
[`web/static/style.css`](../web/static/style.css) as CSS custom properties on `:root`, with a dark
set under `prefers-color-scheme: dark` (guarded by `:root:not([data-theme="light"])`) and again
under `:root[data-theme="dark"]` for the manual toggle. Components only reference tokens, so a
colour change is one edit per theme.

## Principles

1. **Quiet chrome, loud data.** Neutral greys, hairline borders, almost no shadow. Colour is
   reserved for state (success, warning, danger) and for the single accent.
2. **One accent.** Indigo (`--accent`) marks the primary action, focus, the active nav icon,
   selection and the first chart series. Nothing else is indigo.
3. **Numbers line up.** Every figure uses tabular figures in the sans font
   (`font-variant-numeric: tabular-nums`) and is right-aligned in tables. Monospace is only for
   identifiers you might copy: paths, model ids, key masks, request ids, code.
4. **Density without clutter.** 32px controls, 13–14px text, 10px cell padding. Hierarchy comes
   from weight and grey level, not size jumps.
5. **Both themes are first-class.** Dark is not an inversion: surfaces step up in lightness,
   borders are dim, shadows become a faint inner highlight, status colours are lighter.

## Tokens

### Colour

| Token | Light | Dark | Use |
|---|---|---|---|
| `--bg` | `#fafafa` | `#09090b` | Page background, top bar |
| `--bg-sidebar` | `#f6f6f7` | `#09090b` | Sidebar |
| `--surface` | `#ffffff` | `#111114` | Cards, inputs, menus, modals |
| `--surface-2` | `#f6f6f8` | `#17171b` | Segmented track, chips, stat tiles |
| `--surface-3` | `#ececf0` | `#212126` | Progress tracks, neutral fills |
| `--th-bg` | `#fbfbfc` | `#141417` | Table headers, group rows, modal footers |
| `--border` | `#e7e7eb` | `#222227` | Card and control borders |
| `--border-strong` | `#d3d3da` | `#34343c` | Hover borders, checkbox outline |
| `--border-subtle` | `#f0f0f3` | `#1a1a1f` | Row separators inside cards |
| `--text` | `#121216` | `#ededf0` | Primary text, values |
| `--text-2` | `#575762` | `#a3a3ad` | Secondary text, labels |
| `--text-3` | `#8b8b97` | `#6e6e79` | Hints, axis labels, table headers |
| `--accent` | `#5e6ad2` | `#6e78e8` | Primary button, focus, active states |
| `--accent-text` | `#4b56bd` | `#a4aaf7` | Accent used as text (links, active icon) |
| `--accent-soft` / `--accent-ring` | 10% / 22% | 14% / 30% | Selected rows / focus ring |
| `--success` | `#15803d` | `#4cc38a` | 2xx, active, healthy |
| `--warning` | `#b45309` | `#f0b147` | 4xx, cooldown, retries |
| `--danger` | `#d02f2f` | `#f2706b` | 5xx, disabled, destructive |
| `--info` | `#2463d6` | `#5ea8ff` | Streaming tag, notes |

Each status colour has a `-soft` variant (8–12% alpha) for badge and callout backgrounds; text on
a soft background always uses the solid colour.

**Logo.** The mark is monochrome (`--logo-bg` / `--logo-fg`: near-black tile in light, near-white
in dark), so the accent stays free for interaction.

### Charts

Series colours `--c1` … `--c8` are an indigo, teal, amber, pink, sky, lime, violet, orange set with
similar perceived lightness; `--c-other` is grey. A model keeps its colour across the chart and the
breakdown table (`colorFor()` in `charts.js`). Requests are stacked `--chart-ok` (soft indigo) over
`--chart-err` (soft red). Lines are 1.75px, monotone-smoothed (they never overshoot the data);
areas fade from 22% to 0. Gridlines use `--grid`, the baseline uses `--border`. Tooltips are a
blurred surface card with a hairline under the title and tabular values.

### Type

- Sans: `Inter` / `Geist` if installed, then the platform UI font (`-apple-system`,
  `Segoe UI Variable Text`, Roboto), then CJK fonts. `system-ui` comes last on purpose: on
  Chinese-locale Windows it resolves to Microsoft YaHei UI, which would take over Latin text.
- Mono: `Geist Mono`, `JetBrains Mono`, SF Mono, Cascadia Mono, Menlo, Consolas, at 0.9em and weight
  360 (variable monos look heavy at 12px).
- Scale: page title 17/600, card title 14/600, body 13–14/400, labels 12.5/500, table header
  12/500 in `--text-3`, captions 11.5. KPI values 26/600, detail stats 19/600, both with
  −0.025em tracking.
- Section labels inside cards (`.form-section`, `.detail-block h4`) are 11px uppercase with
  0.07em tracking.

### Shape and depth

| Token | Value | Use |
|---|---|---|
| `--radius-xs` | 5px | Badges (status), chips, kind tags |
| `--radius-sm` | 7px | Buttons, inputs, nav items |
| `--radius` | 10px | Tiles, panes, code blocks, toasts, menus |
| `--radius-lg` | 12px | Cards, KPI cards |
| modal / login | 14px / 16px | Overlays |
| `--card-shadow` | 1px soft (light), inner highlight (dark) | Cards |
| `--shadow` | medium | Menus, toasts, chart tooltips |
| `--shadow-lg` | large | Modals, drawers, login card, save bar |

Spacing follows a 4px grid: 8 / 12 / 16 / 20 / 24 / 32. Content padding is 24px 32px on desktop
and 16px on phones; cards are 16px 20px.

### Motion

`--ease: cubic-bezier(.2, .7, .3, 1)`. Pages fade up 4px in 220ms; modals scale from 0.98; drawers
slide 24px; menus and toasts fade in 120–240ms. Hover transitions are 120ms colour changes only.
Everything collapses under `prefers-reduced-motion`.

## Components

- **Buttons** — 32px (`.btn-sm` 28, `.btn-lg` 40). Default: surface + border. Primary: accent with
  an inner top highlight. Danger: solid red for confirmations, `.btn-danger-soft` for row actions.
  Ghost: no border, `--text-2`, hover fill. Icon-only buttons are square.
- **Inputs / selects / tag inputs** — 32px, surface, 1px border, 3px `--accent-ring` focus ring.
  Mono only for keys, URLs and model ids.
- **Checkboxes** — drawn (`appearance: none`), 15px, 4px radius, accent fill with a clipped check.
- **Switch** — 32×18 track, 14px thumb, accent when on, warning when contradicted (`.switch-warn`).
- **Segmented control** — 2px track on `--surface-2`; the active segment is a raised surface chip.
- **Tabs** — text tabs with a 2px `--text` underline (not accent, so tabs never compete with the
  primary button).
- **Badges** — 20px pills; `.badge-dot` for states. HTTP status uses `.badge-status`: 5px radius,
  tabular sans, colour by class (2xx success, 3xx info, 4xx warning, 5xx danger).
- **Tables** — sticky `--th-bg` header in `--text-3`; rows separated by `--border-subtle`; hover
  `--hover`; numbers right-aligned tabular; long text truncated with a `title` tooltip; failed log
  rows carry a 2px red left edge. On phones `.table-cards` turns rows into stacked cards.
- **Cards** — 12px radius, 1px border, header 16px 20px with title + muted subtitle + actions.
- **KPI cards** — label left, small icon right (tinted when the metric has a state), 26px value, one or two caption lines, and a
  34px sparkline pinned to the bottom edge (drawn from the same series as the requests chart;
  narrow ranges such as success rate are zoomed so the trend is visible). The value turns
  warning/danger only when the metric is unhealthy.
- **Drawer / modal** — surface panel, subtle header divider, `--th-bg` footer, `--shadow-lg`,
  2px backdrop blur.
- **Toast** — surface card, coloured icon only; errors get a tinted border.
- **Pagination** — 28px page buttons; the current page is a raised surface chip. The component's
  logic belongs to the page scripts; this file only styles it.
- **Empty states** — 44px icon tile with two faint concentric rings, a title and an optional hint.
- **Login** — centred card on a soft accent glow with a masked dot grid.
- **API docs** — Swagger UI keeps its own markup; operation blocks get a 1px border, 8px radius,
  5% method tint and the console's fonts.

## Adding UI

Use the existing classes before adding new ones. If a new colour seems necessary, add a token to
all three theme blocks instead of hard-coding it. Check every change at 1440px and 390px in both
themes.

## v0.4.0 components

- **Grouped sidebar** — Overview on top, then *Gateway / System / Docs* labels (`.nav-group-label`:
  11px, 600, `--text-3`; uppercase in English, letter-spaced in Chinese). Groups are separated by
  the label's top spacing, not by rules.
- **Account menu** — the sidebar foot is a button (`.me-btn`): 26px avatar with an online dot, name,
  sign-in method, chevron. It opens a `.menu-pop` upwards: change password, admin access, log out.
- **Banners** — `.banner-warning` (initial password) and `.banner-danger` (no upstream key available;
  solid red action button). The danger banner sits on top when both show.
- **Getting-started checklist** — `.onboard` card with a progress bar and four step tiles; only the
  current step is highlighted (accent ring) and carries the one primary button; done steps turn
  `--th-bg` with a green check. 4 → 2 → 1 columns.
- **Entity drawers** (upstream key, client token) — `drawer-lg`; header with status badge and
  `.entity-sub` meta line; `.usage-grid` (24h / 7d / 30d, cost first, errors red when > 0);
  `.quota-block`; `.test-hero` for the last check; `.mini-list` of recent failures linking to logs.
- **Credential labels** — `.cred`: name, then a grey mono prefix (`· sk-xr-nZja`). The name keeps
  priority when space is short; the prefix truncates first.
- **Menu items with an explanation** — second line in `--text-3`; actions that spend credits use
  `--warning` for that line.
- **Pages** — `#/settings` and `#/access` use `.page-narrow` (920px). `#/data` is a two-column grid:
  storage stats across the top, then retention, clean-up (`.estimate` turns danger-soft when rows
  would be deleted), compaction and system status. Numbers there are tabular sans, not mono.
