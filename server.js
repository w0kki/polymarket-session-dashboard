import express from 'express';
import { existsSync, readFileSync, writeFileSync } from 'fs';
import { join } from 'path';

const app = express();
const PORT = process.env.PORT || 5174;
const SETTINGS_FILE = join(import.meta.dirname, 'kcc-settings.json');

app.use(express.json());
app.use(express.static(join(import.meta.dirname, 'dist')));

// KCC settings — persist to disk
app.get('/api/settings', (req, res) => {
  try {
    res.json(existsSync(SETTINGS_FILE)
      ? JSON.parse(readFileSync(SETTINGS_FILE, 'utf8'))
      : {});
  } catch {
    res.json({});
  }
});

app.post('/api/settings', (req, res) => {
  try {
    writeFileSync(SETTINGS_FILE, JSON.stringify(req.body, null, 2));
    res.json({ ok: true });
  } catch {
    res.status(500).json({ error: 'Failed to save settings' });
  }
});

// Proxy Polymarket data API (replaces Vite dev proxy)
app.use('/api/data', async (req, res) => {
  const qs = Object.keys(req.query).length
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

// SPA fallback
app.get('*', (req, res) => {
  res.sendFile(join(import.meta.dirname, 'dist', 'index.html'));
});

app.listen(PORT, '0.0.0.0', () => {
  console.log(`Trading Dashboard running on http://0.0.0.0:${PORT}`);
});
