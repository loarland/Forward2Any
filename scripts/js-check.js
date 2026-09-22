// app.js 里那两段前端逻辑（列表筛选 + 前端翻页）的测试。
//
// 仓库里没有引入 jsdom 这类依赖，所以这里自己搭一个够用的假 DOM：
// 只实现这两段代码真正用到的那几个 API（查元素、属性、hidden、classList、事件）。
// 用 node 跑：node scripts/js-check.js
//
// e2e.sh 会在装了 node 的机器上顺手跑它；没有 node 就跳过。
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const src = fs.readFileSync(path.join(__dirname, '..', 'internal', 'web', 'ui', 'static', 'app.js'), 'utf8');

let failed = 0;
function ok(cond, msg) {
  if (cond) {
    console.log('✅ ' + msg);
  } else {
    failed++;
    console.log('❌ ' + msg);
  }
}

// ---------- 假 DOM ----------

function matches(el, sel) {
  const m = /^([a-z]*)\[([a-zA-Z-]+)(?:=["']?([^"']*)["']?)?\]$/.exec(sel);
  if (!m) {
    throw new Error('这个假 DOM 不认识选择器：' + sel);
  }
  const [, tag, attr, value] = m;
  if (tag && el.tagName !== tag.toUpperCase()) {
    return false;
  }
  if (el.attrs[attr] === undefined) {
    return false;
  }
  return value === undefined || String(el.attrs[attr]) === value;
}

class El {
  constructor(tag, attrs) {
    this.tagName = tag.toUpperCase();
    this.attrs = attrs || {};
    this.children = [];
    this.listeners = {};
    this.hidden = false;
    this.scrolled = 0;
    this.focused = false;
    this.submitted = false;
    this.textContent = '';
    this.value = this.attrs.value !== undefined ? String(this.attrs.value) : '';
    const classes = new Set();
    this.classList = {
      add: (c) => classes.add(c),
      remove: (c) => classes.delete(c),
      contains: (c) => classes.has(c),
      toggle: (c, on) => (on ? classes.add(c) : classes.delete(c)),
    };
    this.classes = classes;
  }
  getAttribute(k) {
    return this.attrs[k] === undefined ? null : String(this.attrs[k]);
  }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  fire(type, ev) {
    (this.listeners[type] || []).forEach((fn) => fn(ev || { preventDefault() {} }));
  }
  append(child) {
    child.parent = this;
    this.children.push(child);
    return child;
  }
  descendants() {
    let out = [];
    this.children.forEach((c) => {
      out.push(c);
      out = out.concat(c.descendants());
    });
    return out;
  }
  querySelector(sel) {
    return this.querySelectorAll(sel)[0] || null;
  }
  querySelectorAll(sel) {
    return this.descendants().filter((el) => matches(el, sel));
  }
  scrollIntoView() {
    this.scrolled++;
  }
  focus() {
    this.focused = true;
  }
  submit() {
    this.submitted = true;
  }
}

function makeDocument(roots) {
  return {
    querySelectorAll(sel) {
      let out = [];
      roots.forEach((r) => {
        out.push(r);
        out = out.concat(r.descendants());
      });
      return out.filter((el) => matches(el, sel));
    },
    querySelector(sel) {
      return this.querySelectorAll(sel)[0] || null;
    },
    addEventListener() {},
  };
}

// ---------- 取 app.js 里第 5 段（列表筛选 + 前端翻页）与 5b（服务端翻页的每页条数） ----------

const start = src.indexOf('  // 5) 列表筛选条');
const end = src.indexOf('  // 6) 设置页的外观实时预览');
if (start < 0 || end < 0) {
  console.error('在 app.js 里找不到第 5 段或第 6 段的标记，测试需要跟着改');
  process.exit(1);
}
const section = src.slice(start, end);

// ---------- 搭一个「源」页面的假 DOM：25 行、每页 20 条 ----------

const SIZE_OPTIONS = [10, 20, 50, 100, 200];

function buildPage(rowCount, perPage, rowSearch) {
  const bar = new El('div', { 'data-list-filter': '' });
  const search = bar.append(new El('input', { 'data-lf-search': '' }));
  bar.append(new El('select', { 'data-lf': 'kind' }));
  const reset = bar.append(new El('button', { 'data-lf-reset': '' }));
  const count = bar.append(new El('span', { 'data-lf-count': '' }));
  reset.classList.add('lf-off');

  const rows = [];
  const list = new El('div', {});
  for (let i = 0; i < rowCount; i++) {
    rows.push(list.append(new El('div', { 'data-row': '', 'data-search': rowSearch(i) })));
  }

  const pager = new El('div', { 'data-pager-client': '', 'data-size': String(perPage) });
  const sizeSel = pager.append(new El('select', { 'data-pg-size': '' }));
  SIZE_OPTIONS.forEach((n) => {
    const o = sizeSel.append(new El('option', { value: String(n) }));
    if (n === perPage) {
      o.attrs.selected = '';
    }
  });
  sizeSel.value = String(perPage);
  const first = pager.append(new El('button', { 'data-pg-first': '' }));
  const prev = pager.append(new El('button', { 'data-pg-prev': '' }));
  const cur = pager.append(new El('span', { 'data-pg-cur': '' }));
  const next = pager.append(new El('button', { 'data-pg-next': '' }));
  const last = pager.append(new El('button', { 'data-pg-last': '' }));
  const jump = pager.append(new El('input', { 'data-pg-input': '' }));
  const pagesEl = pager.append(new El('span', { 'data-pg-pages': '' }));
  const go = pager.append(new El('button', { 'data-pg-go': '' }));

  const empty = new El('div', { 'data-lf-empty': '' });
  empty.hidden = true;

  const doc = makeDocument([bar, list, pager, empty]);
  const ctx = { document: doc, Math, Object, parseInt, isNaN, Array, String, Number };
  vm.createContext(ctx);
  vm.runInContext(section, ctx);

  const visible = () => rows.filter((r) => !r.hidden);
  return { doc, bar, search, reset, count, rows, pager, sizeSel, first, prev, cur, next, last, jump, pagesEl, go, empty, visible };
}

// ---------- 场景 ----------

// 1) 25 行、每页 20 条：第一页 1–20，下一页按钮可用，上一页置灰
let p = buildPage(25, 20, (i) => '源' + i);
ok(p.visible().length === 20, '每页 20 条时第一页显示 20 行');
ok(p.visible()[0] === p.rows[0] && p.visible()[19] === p.rows[19], '第一页显示的是第 1–20 行');
ok(p.cur.textContent === '1', '当前页显示 1');
ok(p.pagesEl.textContent === '2', '25 条按每页 20 条算出 2 页');
ok(p.prev.classes.has('off'), '第一页时「上一页」置灰');
ok(!p.next.classes.has('off'), '第一页时「下一页」可用');
ok(p.jump.value === '1' && p.jump.max === '2', '跳转框回填当前页，上限是总页数');

// 2) 下一页 → 第 21–25 行，下一页置灰
p.next.fire('click');
ok(p.visible().length === 5, '翻到第二页只剩 5 行');
ok(p.visible()[0] === p.rows[20], '第二页从第 21 行开始');
ok(p.cur.textContent === '2', '当前页变成 2');
ok(p.next.classes.has('off') && p.last.classes.has('off'), '最后一页时「下一页 / 末页」置灰');
ok(!p.prev.classes.has('off'), '最后一页时「上一页」可用');
ok(p.rows[20].scrolled + p.rows[0].scrolled > 0, '翻页后把这一页的第一条滚进了视野');

// 3) 首页 / 末页
p.first.fire('click');
ok(p.cur.textContent === '1' && p.visible()[0] === p.rows[0], '「首页」回到第 1 页');
p.last.fire('click');
ok(p.cur.textContent === '2' && p.visible()[0] === p.rows[20], '「末页」跳到最后一页');

// 4) 跳转框：输入 1 再点跳转（以及回车）
p.jump.value = '1';
p.go.fire('click');
ok(p.cur.textContent === '1', '「跳转」按输入框的页码翻页');
p.jump.value = '2';
p.jump.fire('keydown', { key: 'Enter', preventDefault() {} });
ok(p.cur.textContent === '2', '跳转框里按回车也能翻页');
p.jump.value = '999';
p.go.fire('click');
ok(p.cur.textContent === '2', '跳转页超范围时夹到最后一页');
p.jump.value = '0';
p.go.fire('click');
ok(p.cur.textContent === '1', '跳转页填 0 时回到第一页');

// 5) 换每页条数：回到第一页，页数跟着变
p.sizeSel.value = '10';
p.sizeSel.fire('change');
ok(p.cur.textContent === '1' && p.pagesEl.textContent === '3', '换成每页 10 条：回到第 1 页、共 3 页');
ok(p.visible().length === 10, '每页 10 条时第一页显示 10 行');
ok(p.jump.value === '1' && p.jump.max === '3', '换条数后跳转框跟着更新');

// 6) 筛选：只在符合条件的那部分里翻页，计数文案仍是「显示 N / M 条」
p.sizeSel.value = '10';
p.sizeSel.fire('change');
p.search.value = '源1'; // 命中 源1、源10..源19 共 11 行
p.bar.fire('input');
ok(p.count.textContent === '显示 11 / 25 条', '计数文案是「显示 11 / 25 条」，得到 ' + p.count.textContent);
ok(!p.reset.classes.has('lf-off'), '筛选时「清除筛选」可见');
ok(p.pagesEl.textContent === '2', '筛出 11 条按每页 10 条是 2 页');
ok(p.visible().length === 10, '筛选后第一页仍是 10 行');
ok(p.visible().every((r) => r.getAttribute('data-search').indexOf('源1') >= 0), '筛选后显示的行都符合条件');
p.next.fire('click');
ok(p.visible().length === 1, '筛选结果翻到第二页只剩 1 行');

// 7) 筛选条件变化时回到第一页
p.search.value = '源2';
p.bar.fire('input');
ok(p.cur.textContent === '1', '改了筛选条件就回到第 1 页');

// 8) 清除筛选：全部回来，页码归 1
p.reset.fire('click');
ok(p.search.value === '' && p.cur.textContent === '1', '「清除筛选」清空输入并回到第 1 页');
ok(p.count.textContent === '共 25 条', '清除后计数回到「共 25 条」');
ok(p.reset.classes.has('lf-off'), '清除后按钮重新隐藏（位置留着）');
ok(p.visible().length === 10 && p.pagesEl.textContent === '3', '清除后按每页 10 条重新分页');

// 9) 一条都不匹配：给出空状态提示，行全部藏起来
p.search.value = '根本不存在';
p.bar.fire('input');
ok(!p.empty.hidden, '没有匹配时显示空状态');
ok(p.visible().length === 0, '没有匹配时不显示任何行');
ok(p.pagesEl.textContent === '1' && p.cur.textContent === '1', '没有匹配时仍是 1 页');

// 10) 只有一页时不显示翻页按钮的可用态（都置灰）
const one = buildPage(3, 20, (i) => 'x' + i);
ok(one.pagesEl.textContent === '1', '3 条时只有 1 页');
ok(one.prev.classes.has('off') && one.next.classes.has('off'), '只有 1 页时四个方向都不可用');
ok(one.visible().length === 3, '只有 1 页时全部显示');

// 11) 服务端翻页那份：换「每页条数」提交表单，并把页码归到第一页
const form = new El('form', { method: 'get' });
const pageInput = form.append(new El('input', { name: 'page', value: '9' }));
const serverSel = form.append(new El('select', { 'data-pg-submit': '', name: 'per_page' }));
const doc2 = makeDocument([form]);
const ctx2 = { document: doc2, Math, Object, parseInt, isNaN, Array, String, Number };
vm.createContext(ctx2);
vm.runInContext(section, ctx2);
ok(serverSel.form === undefined, '假 DOM 里的下拉没有 form 引用时不该报错');
serverSel.form = form;
serverSel.value = '200';
serverSel.fire('change');
ok(pageInput.value === '1', '换每页条数时把页码归到第 1 页');
ok(form.submitted, '换每页条数时提交了那个 GET 表单');

console.log('');
if (failed) {
  console.log(failed + ' 项不通过');
  process.exit(1);
}
console.log('全部通过 🎉');
