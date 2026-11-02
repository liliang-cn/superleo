// Which conversations a coding agent's card lists.
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const ts = require('typescript');
const src = fs.readFileSync(path.resolve(__dirname, '../src/world/codingSessions.ts'), 'utf8');
const out = ts.transpileModule(src, {compilerOptions: {target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.CommonJS}}).outputText;
const mod = {exports: {}};
new Function('module', 'exports', out)(mod, mod.exports);
const {sessionsOf, splitAgent, folderOf} = mod.exports;

const c = (id, o) => ({id, kind: 'coding', title: id, at: '2026-11-02T10:00:00Z', ...o});
const items = [
  c('cli:a', {agent: 'claude', at: '2026-11-01T10:00:00Z', cwd: '/Users/me/app'}),
  c('cli:b', {agent: 'claude', at: '2026-11-02T10:00:00Z', state: 'running'}),
  c('term:c', {name: 'claude', at: '2026-11-02T08:00:00Z', source: 'terminal'}),
  c('cli:d', {agent: 'claude', machine: 'mac', at: '2026-11-02T11:00:00Z'}),
  c('cli:e', {agent: 'codex', at: '2026-11-02T12:00:00Z'}),
  {id: 'direct:f', kind: 'agent', name: 'claude', title: 'asked', at: '2026-11-02T13:00:00Z'},
  {id: 'queen:g', kind: 'queen', title: 'hi', at: '2026-11-02T14:00:00Z'},
];

test('plain claude lists the core\'s sessions, terminal ones included, newest first', () => {
  assert.deepEqual(sessionsOf(items, 'claude').map((x) => x.id), ['cli:b', 'term:c', 'cli:a']);
});

test('claude.mac lists only the Mac\'s sessions', () => {
  assert.deepEqual(sessionsOf(items, 'claude.mac').map((x) => x.id), ['cli:d']);
});

test('another CLI and conversations that are not coding are left out', () => {
  assert.deepEqual(sessionsOf(items, 'codex').map((x) => x.id), ['cli:e']);
  assert.deepEqual(sessionsOf(items, 'pi'), []);
});

test('the agent id splits at its first dot', () => {
  assert.deepEqual(splitAgent('claude.mac'), {cli: 'claude', machine: 'mac'});
  assert.deepEqual(splitAgent('codex'), {cli: 'codex', machine: ''});
  assert.equal(folderOf({cwd: '/Users/me/app/'}), 'app');
  assert.equal(folderOf({}), '');
});
