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

  document.addEventListener('DOMContentLoaded', syncSourceForm);
  document.addEventListener('change', function (e) {
    if (e.target && (e.target.name === 'kind' || e.target.name === 'usage')) {
      syncSourceForm();
    }
  });

  // 2) 复制回调地址。
  document.addEventListener('click', function (e) {
    if (!e.target || !e.target.closest) {
      return;
    }
    var btn = e.target.closest('[data-copy]');
    if (!btn) {
      return;
    }
    var text = btn.getAttribute('data-copy') || '';

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
