/* topology.js — 服务器↔用户拓扑画布
 *
 * 用法:
 *   <div class="topology"
 *        data-server-addr="vpn.example.com:500"
 *        data-server-state="ok"
 *        data-clients='[{"name":"alice","online":true,"bytesIn":1234}, ...]'>
 *   </div>
 *
 * 自动渲染 SVG + 数据流动画。点击节点 → window.location = 链接。
 */
(function () {
  'use strict';

  function escapeHTML(s) {
    return String(s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function formatBytes(n) {
    if (!n || n <= 0) return '—';
    var units = ['B', 'KB', 'MB', 'GB', 'TB'];
    var i = 0; var v = n;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (i === 0 ? v.toFixed(0) : v.toFixed(1)) + ' ' + units[i];
  }

  function renderTopology(root) {
    var serverAddr = root.getAttribute('data-server-addr') || '';
    var serverState = root.getAttribute('data-server-state') || 'unknown';
    var clientsAttr = root.getAttribute('data-clients') || '[]';
    var focusUser = root.getAttribute('data-focus-user') || '';
    var height = parseInt(root.getAttribute('data-height') || '360', 10);

    var clients;
    try { clients = JSON.parse(clientsAttr) || []; }
    catch (e) { clients = []; }

    // 渲染网格背景
    var svg = '';
    svg += '<svg class="topology-svg" viewBox="0 0 1200 ' + height + '" preserveAspectRatio="xMidYMid meet" xmlns="http://www.w3.org/2000/svg">';

    // 网格
    svg += '<defs>';
    svg += '<pattern id="grid-' + Math.random().toString(36).slice(2, 8) +
           '" width="40" height="40" patternUnits="userSpaceOnUse">';
    svg += '<path d="M 40 0 L 0 0 0 40" fill="none" stroke="rgba(148,163,184,0.15)" stroke-width="1"/>';
    svg += '</pattern>';
    var gridId = svg.match(/id="(grid-[a-z0-9]+)"/)[1];
    svg += '<linearGradient id="flow-' + gridId + '" x1="0" y1="0" x2="1" y2="0">';
    svg += '<stop offset="0%" stop-color="#4f46e5" stop-opacity="0"/>';
    svg += '<stop offset="50%" stop-color="#4f46e5" stop-opacity="0.8"/>';
    svg += '<stop offset="100%" stop-color="#4f46e5" stop-opacity="0"/>';
    svg += '</linearGradient>';
    svg += '<linearGradient id="flow-down-' + gridId + '" x1="0" y1="0" x2="1" y2="0">';
    svg += '<stop offset="0%" stop-color="#10b981" stop-opacity="0"/>';
    svg += '<stop offset="50%" stop-color="#10b981" stop-opacity="0.8"/>';
    svg += '<stop offset="100%" stop-color="#10b981" stop-opacity="0"/>';
    svg += '</linearGradient>';
    svg += '</defs>';

    svg += '<rect width="1200" height="' + height + '" fill="url(#' + gridId + ')"/>';

    var serverX = 600;
    var serverY = 60;
    var serverR = 36;

    // 用户节点排版
    var maxClients = 8;
    var visible = clients.slice(0, maxClients);
    var n = visible.length;
    var userR = 22;
    var userY = height - 60;
    var startX = 200;
    var endX = 1000;

    // 连线
    for (var i = 0; i < n; i++) {
      var ux = n === 1 ? 600 : startX + (endX - startX) * (i / (n - 1));
      // 连线
      svg += '<line x1="' + serverX + '" y1="' + (serverY + serverR) +
             '" x2="' + ux + '" y2="' + (userY - userR) +
             '" stroke="' + (visible[i].online ? '#cbd5e1' : '#e2e8f0') +
             '" stroke-width="' + (visible[i].online ? '2' : '1') + '" stroke-dasharray="' +
             (visible[i].online ? '0' : '4 4') + '"/>';

      // 上行流动 (服务器←用户) — 颜色 emerald (BytesOut)
      if (visible[i].online) {
        svg += '<line class="flow-line flow-up" data-from-x="' + ux +
               '" data-from-y="' + (userY - userR) + '" data-to-x="' + serverX +
               '" data-to-y="' + (serverY + serverR) + '" x1="' + ux +
               '" y1="' + (userY - userR) + '" x2="' + serverX + '" y2="' +
               (serverY + serverR) + '" stroke="url(#flow-down-' + gridId +
               ')" stroke-width="2" stroke-dasharray="8 200" style="animation-duration:' +
               (2 + i * 0.2) + 's"/>';
      }
    }

    // 服务器节点
    var serverColor = serverState === 'ok' ? '#4f46e5' : (serverState === 'warn' ? '#f59e0b' : '#94a3b8');
    svg += '<circle cx="' + serverX + '" cy="' + serverY + '" r="' + (serverR + 6) +
           '" fill="' + serverColor + '" opacity="0.15"/>';
    svg += '<circle cx="' + serverX + '" cy="' + serverY + '" r="' + serverR +
           '" fill="' + serverColor + '" stroke="#fff" stroke-width="3"/>';
    svg += '<text x="' + serverX + '" y="' + (serverY + 5) + '" text-anchor="middle" fill="#fff" font-size="18" font-weight="600">S</text>';
    svg += '<text x="' + serverX + '" y="' + (serverY + serverR + 22) +
           '" text-anchor="middle" fill="#0f172a" font-size="13" font-weight="600">IKEv2 Server</text>';
    svg += '<text x="' + serverX + '" y="' + (serverY + serverR + 38) +
           '" text-anchor="middle" fill="#64748b" font-family="ui-monospace,monospace" font-size="11">' +
           escapeHTML(serverAddr) + '</text>';

    // 用户节点
    for (var j = 0; j < n; j++) {
      var cx = n === 1 ? 600 : startX + (endX - startX) * (j / (n - 1));
      var cy = userY;
      var c = visible[j];
      var isOnline = !!c.online;
      var isFocus = c.name === focusUser;
      var userColor = isOnline ? '#10b981' : '#94a3b8';
      var ringR = isFocus ? userR + 4 : userR;

      if (isFocus) {
        svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + (userR + 8) +
               '" fill="#4f46e5" opacity="0.2"/>';
      }

      svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + ringR +
             '" fill="' + userColor + '" stroke="#fff" stroke-width="3"/>';
      svg += '<text x="' + cx + '" y="' + (cy + 4) + '" text-anchor="middle" fill="#fff" font-size="13" font-weight="600">' +
             (isOnline ? '●' : '○') + '</text>';

      // 名字
      svg += '<text x="' + cx + '" y="' + (cy + userR + 18) +
             '" text-anchor="middle" fill="' + (isFocus ? '#0f172a' : '#475569') +
             '" font-size="12" font-weight="' + (isFocus ? '700' : '500') + '">' +
             escapeHTML(c.name) + '</text>';

      // 流量 / 状态
      var sub;
      if (isOnline && (c.bytesIn || c.bytesOut)) {
        sub = '↓' + formatBytes(c.bytesIn) + ' ↑' + formatBytes(c.bytesOut);
      } else if (isOnline) {
        sub = '在线';
      } else {
        sub = '离线';
      }
      svg += '<text x="' + cx + '" y="' + (cy + userR + 33) + '" text-anchor="middle" fill="#94a3b8" font-size="10" font-family="ui-monospace,monospace">' +
             sub + '</text>';

      // 点击 hitbox
      var href = c.href || ('/users/' + (c.id || ''));
      svg += '<a href="' + escapeHTML(href) + '"><circle cx="' + cx + '" cy="' + cy +
             '" r="' + (userR + 2) + '" fill="transparent" style="cursor:pointer"/></a>';
    }

    // 如果用户数超出
    if (clients.length > maxClients) {
      svg += '<text x="600" y="' + (userY + userR + 50) + '" text-anchor="middle" fill="#94a3b8" font-size="11">+' +
             (clients.length - maxClients) + ' 更多用户…</text>';
    }

    // 图例
    svg += '<g transform="translate(40, 30)">';
    svg += '<circle cx="0" cy="0" r="6" fill="#4f46e5"/><text x="14" y="4" font-size="11" fill="#475569">服务器</text>';
    svg += '<circle cx="100" cy="0" r="6" fill="#10b981"/><text x="114" y="4" font-size="11" fill="#475569">在线用户</text>';
    svg += '<circle cx="200" cy="0" r="6" fill="#94a3b8"/><text x="214" y="4" font-size="11" fill="#475569">离线用户</text>';
    svg += '</g>';

    svg += '</svg>';

    root.innerHTML = svg;

    // 数据流动画 (上行/下行用 stroke-dashoffset)
    var styleId = 'topology-flow-style';
    if (!document.getElementById(styleId)) {
      var s = document.createElement('style');
      s.id = styleId;
      s.textContent =
        '.topology-svg .flow-line {' +
          'animation-name: flow-move;' +
          'animation-iteration-count: infinite;' +
          'animation-timing-function: linear;' +
        '}' +
        '@keyframes flow-move {' +
          'from { stroke-dashoffset: 208; }' +
          'to { stroke-dashoffset: 0; }' +
        '}';
      document.head.appendChild(s);
    }
  }

  function init() {
    var nodes = document.querySelectorAll('.topology[data-clients]');
    for (var i = 0; i < nodes.length; i++) renderTopology(nodes[i]);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();