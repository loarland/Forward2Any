// Forward2Any 后台的少量交互脚本。
// 页面本身是标准的表单 POST + 重定向，这里只做两件锦上添花的事。
(function () {
  'use strict';

  // 1) 按「类型 + 用途」显示源表单里相关的字段组。
  function syncSourceForm() {
    var kindEl = document.querySelector('[name=kind]');
    var usageEl = document.querySelector('[name=usage]');
    if (!kindEl || !usageEl) {
      return;
    }
    var kind = kindEl.value;
    var usage = usageEl.value;
    var recv = usage === 'in' || usage === 'both';
    var send = usage === 'out' || usage === 'both';

    document.querySelectorAll('[data-when]').forEach(function (el) {
      var parts = el.getAttribute('data-when').split(':');
      var kindOk = parts[0] === '*' || parts[0] === kind;
      var useOk = (parts[1] === 'recv' && recv) || (parts[1] === 'send' && send);
      el.hidden = !(kindOk && useOk);
    });
  }

  // 2) 鉴权方式一换，「请求头名 / 密钥」这两格的含义就变了，说明也跟着换。
  //    服务端已经把四种说明都渲染好了（只有当前那种不带 hidden），
  //    这里只负责在选择变化时切换，所以没有 JS 也能看到对的那段。
  var AUTH_HINT = {
    none:        { header: '', secret: '' },
    token:       { header: '密钥放哪个请求头', secret: '两边约定一致即可' },
    hmac_sha256: { header: '签名放哪个请求头', secret: '上游配置的 Webhook Secret' },
    basic:       { header: '这里填用户名', secret: '这里填密码' }
  };
  var AUTH_PLACEHOLDER = {
    none:        { header: '', secret: '' },
    token:       { header: 'X-F2A-Token', secret: '随便一串，两边一样即可' },
    hmac_sha256: { header: 'X-Hub-Signature-256', secret: '' },
    basic:       { header: '用户名，如 f2a', secret: '' }
  };

  function syncAuthForm() {
    var modeEl = document.querySelector('[name=auth_mode]');
    if (!modeEl) {
      return;
    }
    var mode = modeEl.value;
    var hint = AUTH_HINT[mode] || AUTH_HINT.none;
    var ph = AUTH_PLACEHOLDER[mode] || AUTH_PLACEHOLDER.none;

    document.querySelectorAll('[data-auth-note]').forEach(function (el) {
      el.hidden = el.getAttribute('data-auth-note') !== mode;
    });
    // 「不校验」时把两个凭据格藏掉，不然看着像必填。
    // 用 hidden 而不是移除，值照样会随表单提交上去，切回别的模式也不会丢。
    document.querySelectorAll('[data-auth-field]').forEach(function (el) {
      el.hidden = mode === 'none';
    });

    var headerHint = document.querySelector('[data-auth-header-hint]');
    if (headerHint) {
      headerHint.textContent = hint.header;
    }
    var secretHint = document.querySelector('[data-auth-secret-hint]');
    if (secretHint) {
      secretHint.textContent = hint.secret;
    }
    var header = document.querySelector('[name=auth_header]');
    if (header && ph.header) {
      header.placeholder = ph.header;
    }
    var secret = document.querySelector('[name=auth_secret]');
    if (secret && ph.secret) {
      secret.placeholder = ph.secret;
    }
  }

  function syncForms() {
    syncSourceForm();
    syncAuthForm();
  }

  document.addEventListener('DOMContentLoaded', syncForms);
  document.addEventListener('change', function (e) {
    if (!e.target) {
      return;
    }
    if (e.target.name === 'kind' || e.target.name === 'usage') {
      syncSourceForm();
    }
    if (e.target.name === 'auth_mode') {
      syncAuthForm();
    }
  });

  // 2) 原生 file 控件被藏起来了，选完文件要把文件名显示到旁边。
  document.addEventListener('change', function (e) {
    var input = e.target;
    if (!input || input.type !== 'file' || !input.closest) {
      return;
    }
    var field = input.closest('.file-field');
    if (!field) {
      return;
    }
    var nameEl = field.querySelector('[data-file-name]');
    if (nameEl) {
      nameEl.textContent = input.files && input.files.length ? input.files[0].name : '未选择文件';
    }
  });

  // 3) 复制。
  //    两种写法：data-copy 直接带要复制的字符串（回调地址），
  //    data-copy-from 给一个选择器、复制那个元素的 textContent（curl 示例这种多行块，
  //    塞进属性里会被转义得没法看）。转义过的实体在 textContent 上已经还原，粘出来就是原文。
  document.addEventListener('click', function (e) {
    if (!e.target || !e.target.closest) {
      return;
    }
    var btn = e.target.closest('[data-copy], [data-copy-from]');
    if (!btn) {
      return;
    }
    var text = btn.getAttribute('data-copy') || '';
    var from = btn.getAttribute('data-copy-from');
    if (from) {
      var src = document.querySelector(from);
      text = src ? src.textContent : '';
    }

    var flash = function () {
      var old = btn.textContent;
      btn.textContent = '已复制';
      setTimeout(function () { btn.textContent = old; }, 1200);
    };

    // clipboard API 只在安全上下文可用；后台常常跑在明文 HTTP 的内网地址上，
    // 所以保留 execCommand 兜底。
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(flash, function () { fallbackCopy(text, flash); });
      return;
    }
    fallbackCopy(text, flash);
  });

  // 4) 过滤条件的可视化编辑器。
  //    真正提交的还是那个文本框（「路径 操作符 值」一行一条），可视化只是它的一个视图：
  //    改行就重写文本框，改文本就重建行。于是没有 JS 时页面照旧能用，服务端也只认一种格式。
  (function () {
    var root = document.querySelector('[data-filter-editor]');
    if (!root) {
      return;
    }
    var tpl = document.querySelector('[data-filter-row-tpl]');
    var ta = root.querySelector('textarea[name=filters]');
    var visual = root.querySelector('[data-filter-visual]');
    var textBox = root.querySelector('[data-filter-text]');
    var rowsBox = root.querySelector('[data-filter-rows]');
    var emptyBox = root.querySelector('[data-filter-empty]');
    var errorBox = root.querySelector('[data-filter-error]');
    var modeBtn = root.querySelector('[data-filter-mode]');
    var addBtn = root.querySelector('[data-filter-add]');
    if (!tpl || !ta || !visual || !textBox || !rowsBox || !modeBtn || !addBtn) {
      return;
    }

    function rows() {
      return Array.prototype.slice.call(rowsBox.children);
    }

    // 操作符决定了「值」这一格还要不要：exists/not_exists 没有值，别的都必填。
    // 这些元信息写在模板的 <option> 上，免得在 JS 里再抄一份操作符表。
    function syncRow(row) {
      var opEl = row.querySelector('[data-f-op]');
      var opt = opEl.options[opEl.selectedIndex];
      var valEl = row.querySelector('[data-f-value]');
      var noValue = !!(opt && opt.hasAttribute('data-novalue'));
      valEl.disabled = noValue;
      if (!noValue) {
        valEl.placeholder = (opt && opt.getAttribute('data-ph')) || '值，如 push / true / 123';
      }
      row.classList.toggle('no-value', noValue);
    }

    function addRow(f) {
      var row = tpl.content.firstElementChild.cloneNode(true);
      row.querySelector('[data-f-path]').value = f.path || '';
      row.querySelector('[data-f-op]').value = f.op || 'eq';
      row.querySelector('[data-f-value]').value = f.value == null ? '' : f.value;
      syncRow(row);
      rowsBox.appendChild(row);
      return row;
    }

    function toText() {
      var out = [];
      rows().forEach(function (row) {
        var path = row.querySelector('[data-f-path]').value.trim();
        if (path === '') {
          return; // 空行等于没写，别生成一条语法错误的记录
        }
        var op = row.querySelector('[data-f-op]').value;
        var val = row.querySelector('[data-f-value]').value.trim();
        out.push(val === '' ? path + ' ' + op : path + ' ' + op + ' ' + val);
      });
      return out.join('\n');
    }

    function fromText(text) {
      rowsBox.innerHTML = '';
      text.split('\n').forEach(function (line) {
        line = line.trim();
        if (line === '' || line.charAt(0) === '#') {
          return;
        }
        var m = line.match(/^(\S+)\s+(\S+)(?:\s+([\s\S]+))?$/);
        if (m) {
          addRow({ path: m[1], op: m[2], value: (m[3] || '').trim() });
        }
      });
    }

    function chrome() {
      var n = rowsBox.children.length;
      emptyBox.hidden = n > 0;
      // 行号要跟着增删走，服务端报错说的「第 N 行」才对得上。
      rows().forEach(function (row, i) {
        row.querySelector('.filter-idx').textContent = i + 1;
      });
      if (n === 0) {
        errorBox.hidden = true;
      }
    }

    function refresh() {
      ta.value = toText();
      chrome();
      // 用户开始改哪一行，就把哪一行的红框撤掉，别让他改完还看着像是错的。
      if (!rowsBox.querySelector('.filter-row.invalid')) {
        errorBox.hidden = true;
      }
    }

    // 两种模式互斥，统一从这里切。之前初始化和点击各写一份显隐逻辑，
    // 结果初始化只打开了可视化那半、忘了关掉文本框，新建规则时两个一起露出来。
    function setMode(toText) {
      visual.hidden = toText;
      textBox.hidden = !toText;
      modeBtn.textContent = toText ? '可视化模式' : '文本模式';
    }

    // 初始化：把文本框里的内容铺成行。注意这里不回写 ta，
    // 否则注释和用户自己的排版在打开页面时就被抹掉了。
    fromText(ta.value);
    chrome();
    setMode(false);
    modeBtn.hidden = false;

    modeBtn.addEventListener('click', function () {
      var toText = !visual.hidden;
      if (toText) {
        setMode(true);
        ta.focus();
        return;
      }
      fromText(ta.value);
      chrome();
      setMode(false);
    });

    ta.addEventListener('input', function () {
      if (!textBox.hidden) {
        fromText(ta.value);
        chrome();
      }
    });

    addBtn.addEventListener('click', function () {
      addRow({}).querySelector('[data-f-path]').focus();
      refresh();
    });

    rowsBox.addEventListener('click', function (e) {
      var btn = e.target.closest ? e.target.closest('[data-f-del]') : null;
      if (!btn) {
        return;
      }
      btn.closest('.filter-row').remove();
      refresh();
    });

    rowsBox.addEventListener('input', function (e) {
      var row = e.target && e.target.closest ? e.target.closest('.filter-row') : null;
      if (row) {
        row.classList.remove('invalid');
      }
      refresh();
    });
    rowsBox.addEventListener('change', function (e) {
      if (e.target && e.target.hasAttribute('data-f-op')) {
        syncRow(e.target.closest('.filter-row'));
      }
      refresh();
    });

    var form = root.closest('form');
    if (form) {
      form.addEventListener('submit', function (e) {
        if (visual.hidden) {
          return; // 文本模式原样交给服务端校验
        }
        var bad = null;
        rows().forEach(function (row) {
          row.classList.remove('invalid');
          if (row.querySelector('[data-f-path]').value.trim() === '') {
            return;
          }
          var valEl = row.querySelector('[data-f-value]');
          if (!valEl.disabled && valEl.value.trim() === '') {
            row.classList.add('invalid');
            bad = bad || row;
          }
        });
        if (!bad) {
          return;
        }
        e.preventDefault();
        errorBox.textContent = '有条件的「值」还没填，补齐后再保存';
        errorBox.hidden = false;
        bad.querySelector('[data-f-value]').focus();
      });
    }
  })();

  // 5) 列表筛选条（源、规则）。
  //    这些列表本来就已经整份渲染在页面上了，所以直接在前端过滤，不发请求：
  //    敲一个字就立刻见效。投递日志不走这里 —— 它的记录会一直涨、还要分页，
  //    筛选取的是服务端那一套。
  document.querySelectorAll('[data-list-filter]').forEach(function (bar) {
    var rows = Array.prototype.slice.call(document.querySelectorAll('[data-row]'));
    if (!rows.length) {
      return;
    }
    var search = bar.querySelector('[data-lf-search]');
    var selects = Array.prototype.slice.call(bar.querySelectorAll('select[data-lf]'));
    var reset = bar.querySelector('[data-lf-reset]');
    var count = bar.querySelector('[data-lf-count]');
    var empty = document.querySelector('[data-lf-empty]');

    function apply() {
      var terms = (search ? search.value : '').toLowerCase().split(/\s+/).filter(Boolean);
      var shown = 0;

      rows.forEach(function (row) {
        // 多个关键词是「都要出现」，跟搜索框的直觉一致。
        var hay = (row.getAttribute('data-search') || '').toLowerCase();
        var ok = terms.every(function (t) { return hay.indexOf(t) >= 0; });

        if (ok) {
          ok = selects.every(function (sel) {
            var want = sel.value;
            return !want || row.getAttribute('data-' + sel.getAttribute('data-lf')) === want;
          });
        }
        row.hidden = !ok;
        if (ok) {
          shown++;
        }
      });

      var filtering = terms.length > 0 || selects.some(function (s) { return s.value !== ''; });
      if (count) {
        count.textContent = filtering
          ? '显示 ' + shown + ' / ' + rows.length + ' 条'
          : '共 ' + rows.length + ' 条';
      }
      if (reset) {
        reset.hidden = !filtering;
      }
      if (empty) {
        empty.hidden = shown > 0;
      }
    }

    bar.addEventListener('input', apply);
    bar.addEventListener('change', apply);
    if (reset) {
      reset.addEventListener('click', function () {
        if (search) {
          search.value = '';
        }
        selects.forEach(function (s) { s.value = ''; });
        apply();
        if (search) {
          search.focus();
        }
      });
    }
    apply();
  });

  // 6) 设置页的外观实时预览。
  //    只改 <html data-theme> 和那一个 <link id=palette> 的 href —— 布局里的
  //    href 本来就是服务端按当前设置渲染的，所以这段纯粹是「还没保存也能看」。
  //    不保存就离开页面的话，预览随页面一起丢掉，不会串到别的页面。
  (function () {
    var colorEl = document.querySelector('[data-theme-color]');
    var modeEl = document.querySelector('[data-theme-mode]');
    var link = document.getElementById('palette');
    if (!colorEl && !modeEl) {
      return;
    }
    function apply() {
      if (link && colorEl) {
        link.href = '/static/palettes/' + colorEl.value + '.css';
      }
      if (modeEl) {
        // auto 时不写这个属性，让 Pico 去跟随 prefers-color-scheme。
        if (modeEl.value === 'auto') {
          document.documentElement.removeAttribute('data-theme');
        } else {
          document.documentElement.setAttribute('data-theme', modeEl.value);
        }
      }
    }
    if (colorEl) {
      colorEl.addEventListener('change', apply);
    }
    if (modeEl) {
      modeEl.addEventListener('change', apply);
    }
    apply();
  })();

  // 7) 规则表单的「选源」列表：搜索 + 全选/清空 + 实时计数。
  //    勾选框本身就是普通控件，没有这段也能勾、也能提交；
  //    工具条是先渲染成 hidden 再由这里放出来的（跟筛选条、文件框一个路子），
  //    所以没 JS 时不会留下一排点了没反应的按钮。
  document.querySelectorAll('[data-picker]').forEach(function (col) {
    var rows = Array.prototype.slice.call(col.querySelectorAll('.pick'));
    if (!rows.length) {
      return;
    }
    var count = col.querySelector('[data-picker-count]');
    var tools = col.querySelector('[data-picker-tools]');
    var search = col.querySelector('[data-picker-search]');
    var empty = col.querySelector('[data-picker-empty]');

    function boxes() {
      return rows.map(function (row) { return row.querySelector('input[type=checkbox]'); });
    }
    function syncCount() {
      if (!count) {
        return;
      }
      var n = boxes().filter(function (b) { return b.checked; }).length;
      count.textContent = n ? '已选 ' + n + ' 个' : '未选择';
      if (n) {
        count.setAttribute('data-on', '1');
      } else {
        count.removeAttribute('data-on');
      }
    }
    // 搜索框一敲就生效，和列表页的筛选条同一个套路：多个关键词是「都要出现」。
    function applyFilter() {
      var terms = (search ? search.value : '').toLowerCase().split(/\s+/).filter(Boolean);
      var shown = 0;
      rows.forEach(function (row) {
        var hay = (row.getAttribute('data-search') || '').toLowerCase();
        var ok = terms.every(function (t) { return hay.indexOf(t) >= 0; });
        row.hidden = !ok;
        if (ok) {
          shown++;
        }
      });
      if (empty) {
        empty.hidden = shown > 0;
      }
    }
    // 「全选」只作用于当前看得见的行：搜索之后它就等于「选中搜出来的这些」。
    function setVisible(checked) {
      rows.forEach(function (row) {
        if (!row.hidden) {
          row.querySelector('input[type=checkbox]').checked = checked;
        }
      });
      syncCount();
    }

    var all = col.querySelector('[data-picker-all]');
    var none = col.querySelector('[data-picker-none]');
    if (all) {
      all.addEventListener('click', function () { setVisible(true); });
    }
    if (none) {
      none.addEventListener('click', function () { setVisible(false); });
    }
    if (tools) {
      tools.hidden = false;
    }
    col.addEventListener('change', syncCount);
    if (search) {
      search.addEventListener('input', applyFilter);
    }
    applyFilter();
    syncCount();
  });

  function fallbackCopy(text, onDone) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try {
      if (document.execCommand('copy')) {
        onDone();
      }
    } catch (err) {
      /* 复制失败就当作没点过，用户还能手动选 */
    }
    document.body.removeChild(ta);
  }
})();
