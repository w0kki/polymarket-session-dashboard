import express from 'express';
import { existsSync, readFileSync, unlinkSync } from 'fs';
import { join } from 'path';

import { getSetting, setSetting, getAllSettings, getTrades, getTradeStats, getLastSync } from './db.js';
import { runSync } from './sync.js';

const app  = express();
const PORT = process.env.PORT || 5174;

// ─── Migrate legacy kcc-settings.json → SQLite settings table (one-time) ──────

const LEGACY_SETTINGS = join(import.meta.dirname, 'kcc-settings.json');
if (existsSync(LEGACY_SETTINGS)) {
  try {
    const data = JSON.parse(readFileSync(LEGACY_SETTINGS, 'utf8'));
    for (const [k, v] of Object.entries(data)) setSetting(k, v);
    unlinkSync(LEGACY_SETTINGS);
    console.log('[settings] Migrated kcc-settings.json → SQLite');
  } catch (e) {
    console.warn('[settings] Migration failed:', e.message);
  }
}

// ─── Background sync ──────────────────────────────────────────────────────────

const SYNC_INTERVAL = 60 * 60 * 1000; // 1 hour

runSync().catch(() => {}); // initial sync on startup (errors logged inside runSync)
setInterval(() => runSync().catch(() => {}), SYNC_INTERVAL);

// ─── Express setup ────────────────────────────────────────────────────────────

app.use(express.json());
app.use(express.static(join(import.meta.dirname, 'dist')));

// ─── KCC / app settings ───────────────────────────────────────────────────────

app.get('/api/settings', (_req, res) => {
  try {
    res.json(getAllSettings());
  } catch {
    res.json({});
  }
});

app.post('/api/settings', (req, res) => {
  try {
    for (const [k, v] of Object.entries(req.body)) setSetting(k, v);
    res.json({ ok: true });
  } catch {
    res.status(500).json({ error: 'Failed to save settings' });
  }
});

// ─── Trades (from SQLite) ─────────────────────────────────────────────────────

app.get('/api/trades', (req, res) => {
  try {
    const { sport, outcome, from, to, limit } = req.query;
    const rows = getTrades({
      sport:   sport   || undefined,
      outcome: outcome || undefined,
      from:    from    || undefined,
      to:      to      || undefined,
      limit:   limit   ? Number(limit) : 500,
    });
    res.json(rows);
  } catch {
    res.status(500).json({ error: 'DB error' });
  }
});

app.get('/api/trades/stats', (_req, res) => {
  try {
    res.json(getTradeStats());
  } catch {
    res.status(500).json({ error: 'DB error' });
  }
});

// ─── Sync status ──────────────────────────────────────────────────────────────

app.get('/api/sync/status', (_req, res) => {
  try {
    res.json(getLastSync() ?? { status: 'never' });
  } catch {
    res.status(500).json({ error: 'DB error' });
  }
});

app.post('/api/sync/trigger', async (_req, res) => {
  try {
    const result = await runSync();
    res.json({ ok: true, ...result });
  } catch (e) {
    res.status(502).json({ error: e?.message ?? 'Sync failed' });
  }
});

// ─── Polymarket CLOB proxy ────────────────────────────────────────────────────

app.use('/api/clob', async (req, res) => {
  const qs  = Object.keys(req.query).length
    ? '?' + new URLSearchParams(req.query).toString()
    : '';
  const url = `https://clob.polymarket.com${req.path}${qs}`;
  try {
    const resp = await fetch(url, { headers: { 'User-Agent': 'Mozilla/5.0' } });
    const data = await resp.json();
    res.status(resp.status).json(data);
  } catch {
    res.status(502).json({ error: 'Proxy error' });
  }
});

// ─── Polymarket data proxy ────────────────────────────────────────────────────

app.use('/api/data', async (req, res) => {
  const qs  = Object.keys(req.query).length
    ? '?' + new URLSearchParams(req.query).toString()
    : '';
  const url = `https://data-api.polymarket.com${req.path}${qs}`;
  try {
    const resp = await fetch(url, { headers: { 'User-Agent': 'Mozilla/5.0' } });
    const data = await resp.json();
    res.status(resp.status).json(data);
  } catch {
    res.status(502).json({ error: 'Proxy error' });
  }
});

// ─── SPA fallback ─────────────────────────────────────────────────────────────

app.use((req, res) => {
  res.sendFile(join(import.meta.dirname, 'dist', 'index.html'));
});

app.listen(PORT, '0.0.0.0', () => {
  console.log(`Trading Dashboard running on http://0.0.0.0:${PORT}`);
});
