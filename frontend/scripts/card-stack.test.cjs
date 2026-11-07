// The 3D view's notification stack: timing, hover and open pauses, "+N", dedupe.
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const ts = require('typescript');
const src = fs.readFileSync(path.resolve(__dirname, '../src/lib/cardStack.ts'), 'utf8');
const out = ts.transpileModule(src, {compilerOptions: {target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.CommonJS}}).outputText;
const mod = {exports: {}};
new Function('module', 'exports', out)(mod, mod.exports);
const {createStack, AUTO_MS} = mod.exports;

// A clock that only moves when told to, with timers that fire in order.
function rig(opts = {}) {
  let t = 1000, seq = 0;
  const timers = new Map();
  const removed = [];
  const s = createStack({
    now: () => t,
    setTimer: (fn, ms) => { const h = ++seq; timers.set(h, {fn, at: t + ms}); return h; },
    clearTimer: (h) => timers.delete(h),
    onRemove: (c, r) => removed.push([c.id, r]),
    ...opts,
  });
  const advance = (ms) => {
    const end = t + ms;
    for (;;) {
      const due = [...timers.entries()].filter(([, x]) => x.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if (!due) break;
      timers.delete(due[0]); t = due[1].at; due[1].fn();
    }
    t = end;
  };
  return {s, advance, removed, timers};
}
const card = (id, o = {}) => ({id, kind: 'notice', level: 'info', title: id, preview: id, body: id, ...o});
const ids = (s) => s.state().cards.map((c) => c.id);

test('newest first; info and success leave by themselves, warn and error stay', () => {
  const {s, advance, removed} = rig();
  s.push(card('a', {level: 'info'}));
  s.push(card('b', {level: 'success'}));
  s.push(card('c', {level: 'warn'}));
  s.push(card('d', {level: 'error'}));
  assert.deepEqual(ids(s), ['d', 'c', 'b', 'a']);
  advance(AUTO_MS + 1);
  assert.deepEqual(ids(s), ['d', 'c']);
  assert.deepEqual(removed, [['a', 'timeout'], ['b', 'timeout']]);
  advance(10 * 60 * 1000);
  assert.deepEqual(ids(s), ['d', 'c']);
});

test('an approval never times out and the x cannot remove it', () => {
  const {s, advance, removed} = rig();
  s.push(card('p', {kind: 'approval', level: 'warn'}));
  s.push(card('n'));
  advance(60000);
  s.dismiss('p');
  s.dismissAll();
  assert.deepEqual(ids(s), ['p']);
  s.resolve('p');
  assert.deepEqual(ids(s), []);
  assert.deepEqual(removed.at(-1), ['p', 'resolved']);
});

test('hover pauses the timer and keeps what was left of it', () => {
  const {s, advance} = rig();
  s.push(card('a'));
  advance(3000);
  s.setHover('a', true);
  advance(60000);
  assert.deepEqual(ids(s), ['a']);
  s.setHover('a', false);
  // 2000 ms were left; a hover does not hand out a new full life.
  advance(1900);
  assert.deepEqual(ids(s), ['a']);
  advance(200);
  assert.deepEqual(ids(s), []);
});

test('a hover that ends with a sliver left still gives the card a moment', () => {
  const {s, advance} = rig();
  s.push(card('a'));
  advance(AUTO_MS - 100);
  s.setHover('a', true);
  s.setHover('a', false);
  advance(1000);
  assert.deepEqual(ids(s), ['a']);
  advance(600);
  assert.deepEqual(ids(s), []);
});

test('an expanded card never times out, and starts counting again when closed', () => {
  const {s, advance} = rig();
  s.push(card('a'));
  s.toggleExpanded('a');
  advance(10 * 60 * 1000);
  assert.deepEqual(ids(s), ['a']);
  assert.deepEqual(s.state().expanded, ['a']);
  s.toggleExpanded('a');
  advance(AUTO_MS + 1);
  assert.deepEqual(ids(s), []);
});

test('hovering an expanded card and leaving does not restart its clock', () => {
  const {s, advance} = rig();
  s.push(card('a'));
  s.toggleExpanded('a');
  s.setHover('a', true);
  s.setHover('a', false);
  advance(AUTO_MS * 4);
  assert.deepEqual(ids(s), ['a']);
});

test('more than four fold into +N, and opening up shows all of them', () => {
  const {s} = rig();
  for (const id of ['a', 'b', 'c', 'd', 'e', 'f']) s.push(card(id, {level: 'error'}));
  let st = s.state();
  assert.deepEqual(st.visible.map((c) => c.id), ['f', 'e', 'd', 'c']);
  assert.equal(st.hidden, 2);
  s.setShowAll(true);
  st = s.state();
  assert.equal(st.visible.length, 6);
  assert.equal(st.hidden, 0);
  s.dismiss('a'); s.dismiss('b'); s.dismiss('c');
  // Back to four or fewer: the stack closes itself up again.
  assert.equal(s.state().showAll, false);
});

test('a card folded into +N still times out', () => {
  const {s, advance} = rig();
  s.push(card('old', {level: 'info'}));
  for (const id of ['a', 'b', 'c', 'd']) s.push(card(id, {level: 'error'}));
  assert.equal(s.state().hidden, 1);
  advance(AUTO_MS + 1);
  assert.deepEqual(ids(s), ['d', 'c', 'b', 'a']);
  assert.equal(s.state().hidden, 0);
});

test('the same id replaces the card, moves it to the top and restarts its life', () => {
  const {s, advance} = rig();
  s.push(card('a', {title: 'first'}));
  s.push(card('b'));
  advance(4000);
  s.push(card('a', {title: 'second'}));
  assert.deepEqual(ids(s), ['a', 'b']);
  assert.equal(s.state().cards[0].title, 'second');
  advance(2000);
  assert.deepEqual(ids(s), ['a']);
  advance(3100);
  assert.deepEqual(ids(s), []);
});

test('dismiss-all removes every dismissable card and tells the sources', () => {
  const {s, removed} = rig();
  s.push(card('a', {level: 'error'}));
  s.push(card('b', {level: 'warn'}));
  s.push(card('p', {kind: 'approval', level: 'warn'}));
  s.dismissAll();
  assert.deepEqual(ids(s), ['p']);
  assert.deepEqual(removed.map((r) => r[1]), ['dismiss', 'dismiss']);
});

test('reset empties the stack silently and stops every timer', () => {
  const {s, removed, timers} = rig();
  s.push(card('a'));
  s.push(card('b', {level: 'error'}));
  s.reset();
  assert.deepEqual(ids(s), []);
  assert.equal(removed.length, 0);
  assert.equal(timers.size, 0);
});

test('subscribers hear every change and the snapshot is stable between them', () => {
  const {s} = rig();
  let n = 0;
  const off = s.subscribe(() => n++);
  s.push(card('a', {level: 'error'}));
  const one = s.state();
  assert.equal(s.state(), one);
  s.toggleExpanded('a');
  assert.notEqual(s.state(), one);
  off();
  s.dismiss('a');
  assert.equal(n, 3 - 1);
});
