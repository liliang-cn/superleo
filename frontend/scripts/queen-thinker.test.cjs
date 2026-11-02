// Who the status bar and the 3D queen say is thinking for her.
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const ts = require('typescript');
const src = fs.readFileSync(path.resolve(__dirname, '../src/lib/format.ts'), 'utf8');
const out = ts.transpileModule(src, {compilerOptions: {target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.CommonJS}}).outputText;
const mod = {exports: {}};
new Function('module', 'exports', 'require', out)(mod, mod.exports, () => ({translate: (s) => s, ClipboardSetText: () => {}}));
const {queenThinker, queenEngineNote} = mod.exports;

test('no engine: SuperLeo thinks, and the model says so', () => {
  assert.equal(queenThinker('', ''), '');
  assert.equal(queenThinker(undefined, 'superleo'), '');
});

test('an engine set and nothing answered yet: the engine, not the idle model', () => {
  assert.equal(queenThinker('hermes.mac', ''), 'hermes · mac');
});

test('SuperLeo answered the latest turn in the engine\'s place: SuperLeo, by its model', () => {
  assert.equal(queenThinker('hermes.mac', 'superleo'), '');
  assert.equal(queenEngineNote('hermes.mac', 'superleo'), 'SuperLeo');
});

test('a later engine in the list answered: that one', () => {
  assert.equal(queenThinker('hermes.mac', 'claude'), 'Claude Code');
});

test('recalled memory handed over as a user message is never a bubble', () => {
  const {isInjectedMemory} = mod.exports;
  assert.equal(isInjectedMemory('user', '## Relevant Memory\n\n- likes tea'), true);
  assert.equal(isInjectedMemory('user', '你现在是 gemini吗？'), false);
  assert.equal(isInjectedMemory('assistant', '## Relevant Memory'), false);
});
