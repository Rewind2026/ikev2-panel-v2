/* topology.js — 服务器↔用户拓扑画布(深色霓虹风)
 *
 * 用法(契约不变):
 *   <div class="topology"
 *        data-server-addr="vpn.example.com:500"
 *        data-server-state="ok"
 *        data-clients='[{"name":"alice","online":true,"bytesIn":1234}, ...]'
 *        data-focus-user="alice"
 *        data-height="360">
 *   </div>
 *
 * 视觉:深底点阵 + 紫色 server 节点 + 青色 online 节点 + 灰色 offline + 双向虚线
 *      + 入场缩放/呼吸 + 数据流光点动画 + 悬停反馈 + 兼容 prefers-reduced-motion
 *      + 响应式(<540px 缩小节点 + 标签字号)
 */
(function () {
  'use strict';

  // 颜色 token(与 style.css --ikev2-* 对齐;此处硬编码以便 SVG 引用)
  var C = {
    bgTop:        'rgba(8, 14, 32, 0.55)',
    bgBottom:     'rgba(2, 6, 16, 0.85)',
    dotGrid:      'rgba(125, 211, 252, 0.07)',
    gridGlow:     'rgba(34, 211, 238, 0.04)',
    server:       '#8b5cf6',     // violet 500 — server 节点主色
    serverHi:     '#a78bfa',
    serverRing:   'rgba(139, 92, 246, 0.55)',
    serverHalo:   'rgba(139, 92, 246, 0.18)',
    online:       '#22d3ee',     // cyan 500 — 在线用户
    onlineHi:     '#67e8f9',
    onlineHalo:   'rgba(34, 211, 238, 0.35)',
    offline:      'rgba(148, 163, 184, 0.55)',  // slate 灰
    offlineHi:    '#94a3b8',
    link:         'rgba(139, 92, 246, 0.45)',
    linkOnline:   'rgba(34, 211, 238, 0.55)',
    linkDash:     '5 5',
    focusGlow:    'rgba(34, 211, 238, 0.30)',
    flowUp:       '#22d3ee',
    flowDown:     '#8b5cf6',
    textPrimary:  'rgba(226, 232, 240, 0.92)',
    textMuted:    'rgba(148, 163, 184, 0.85)',
    textMono:     'rgba(125, 211, 252, 0.80)',
  };

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

  function getResponsiveScale(root) {
    var w = root.clientWidth || root.parentNode.clientWidth || 1200;
    // 三档断点:桌面≥720 / 平板 480-720 / 移动<480
    if (w < 480) return { scale: 0.62, labelFS: 10, subFS: 9, serverFS: 16, userFS: 11, legendFS: 9, maxClients: 5 };
    if (w < 720) return { scale: 0.80, labelFS: 11, subFS: 10, serverFS: 17, userFS: 12, legendFS: 10, maxClients: 6 };
    return                { scale: 1.00, labelFS: 12, subFS: 10, serverFS: 18, userFS: 13, legendFS: 11, maxClients: 8 };
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

    var rsp = getResponsiveScale(root);
    var W = 1200;
    var H = Math.round(height * rsp.scale);
    var uid = Math.random().toString(36).slice(2, 8);

    var serverX = 600;
    var serverY = Math.max(60, Math.round(H * 0.16));
    var serverR = Math.round(34 * rsp.scale);

    var maxClients = rsp.maxClients;
    var visible = clients.slice(0, maxClients);
    var n = visible.length;
    var userR = Math.round(20 * rsp.scale);
    var userY = H - Math.round(50 * rsp.scale);
    var sidePad = Math.round(140 * rsp.scale);
    var startX = sidePad;
    var endX = W - sidePad;

    var svg = '';
    svg += '<svg class="topology-svg" viewBox="0 0 ' + W + ' ' + H +
           '" preserveAspectRatio="xMidYMid meet" role="img" aria-label="IKEv2 服务器连接拓扑" xmlns="http://www.w3.org/2000/svg">';

    // === defs ===
    svg += '<defs>';

    // 深底径向渐变
    svg += '<radialGradient id="bg-' + uid + '" cx="50%" cy="40%" r="80%">';
    svg += '<stop offset="0%" stop-color="' + C.bgTop + '"/>';
    svg += '<stop offset="100%" stop-color="' + C.bgBottom + '"/>';
    svg += '</radialGradient>';

    // 点阵 pattern
    svg += '<pattern id="dots-' + uid + '" width="24" height="24" patternUnits="userSpaceOnUse">';
    svg += '<circle cx="1" cy="1" r="1" fill="' + C.dotGrid + '"/>';
    svg += '</pattern>';

    // 网格线 pattern(更大尺度)
    svg += '<pattern id="grid-' + uid + '" width="120" height="120" patternUnits="userSpaceOnUse">';
    svg += '<path d="M 120 0 L 0 0 0 120" fill="none" stroke="' + C.gridGlow + '" stroke-width="1"/>';
    svg += '</pattern>';

    // server halo gradient
    svg += '<radialGradient id="server-halo-' + uid + '" cx="50%" cy="50%" r="50%">';
    svg += '<stop offset="0%" stop-color="' + C.server + '" stop-opacity="0.55"/>';
    svg += '<stop offset="60%" stop-color="' + C.server + '" stop-opacity="0.18"/>';
    svg += '<stop offset="100%" stop-color="' + C.server + '" stop-opacity="0"/>';
    svg += '</radialGradient>';

    // server body 渐变(玻璃感:顶部高光 → 主紫 → 底部深紫)
    svg += '<linearGradient id="server-body-' + uid + '" x1="0" y1="0" x2="0" y2="1">';
    svg += '<stop offset="0%" stop-color="#a78bfa"/>';
    svg += '<stop offset="45%" stop-color="#8b5cf6"/>';
    svg += '<stop offset="100%" stop-color="#6d28d9"/>';
    svg += '</linearGradient>';

    // online halo gradient
    svg += '<radialGradient id="online-halo-' + uid + '" cx="50%" cy="50%" r="50%">';
    svg += '<stop offset="0%" stop-color="' + C.online + '" stop-opacity="0.50"/>';
    svg += '<stop offset="60%" stop-color="' + C.online + '" stop-opacity="0.15"/>';
    svg += '<stop offset="100%" stop-color="' + C.online + '" stop-opacity="0"/>';
    svg += '</radialGradient>';

    // 流动光点渐变 — server 方向(下行)
    svg += '<linearGradient id="flow-down-' + uid + '" x1="0" y1="1" x2="0" y2="0" gradientUnits="objectBoundingBox">';
    svg += '<stop offset="0%" stop-color="' + C.flowDown + '" stop-opacity="0"/>';
    svg += '<stop offset="50%" stop-color="' + C.flowDown + '" stop-opacity="0.95"/>';
    svg += '<stop offset="100%" stop-color="' + C.flowDown + '" stop-opacity="0"/>';
    svg += '</linearGradient>';

    // 流动光点渐变 — user 方向(上行)
    svg += '<linearGradient id="flow-up-' + uid + '" x1="0" y1="0" x2="0" y2="1" gradientUnits="objectBoundingBox">';
    svg += '<stop offset="0%" stop-color="' + C.flowUp + '" stop-opacity="0"/>';
    svg += '<stop offset="50%" stop-color="' + C.flowUp + '" stop-opacity="0.95"/>';
    svg += '</linearGradient>';

    // server 节点对角切角 clip(可选,做出八边形)
    svg += '<filter id="node-glow-' + uid + '" x="-50%" y="-50%" width="200%" height="200%">';
    svg += '<feGaussianBlur stdDeviation="3" result="blur"/>';
    svg += '<feMerge><feMergeNode in="blur"/><feMergeNode in="SourceGraphic"/></feMerge>';
    svg += '</filter>';

    // LED 灯珠柔光
    svg += '<filter id="led-glow-' + uid + '" x="-100%" y="-100%" width="300%" height="300%">';
    svg += '<feGaussianBlur stdDeviation="1.6" result="b"/>';
    svg += '<feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge>';
    svg += '</filter>';

    svg += '</defs>';

    // === background layers ===
    svg += '<rect width="' + W + '" height="' + H + '" fill="url(#bg-' + uid + ')"/>';
    svg += '<rect width="' + W + '" height="' + H + '" fill="url(#grid-' + uid + ')"/>';
    svg += '<rect width="' + W + '" height="' + H + '" fill="url(#dots-' + uid + ')"/>';

    // === legend (top-left) ===
    var legendY = 28;
    svg += '<g class="topology-legend" transform="translate(28, ' + legendY + ')">';
    svg += '<circle cx="6" cy="0" r="5" fill="' + C.server + '"/>' +
           '<text x="18" y="4" font-size="' + rsp.legendFS + '" fill="' + C.textMuted + '" font-weight="500">服务器</text>';
    svg += '<circle cx="86" cy="0" r="5" fill="' + C.online + '"/>' +
           '<text x="98" y="4" font-size="' + rsp.legendFS + '" fill="' + C.textMuted + '" font-weight="500">在线用户</text>';
    svg += '<circle cx="176" cy="0" r="5" fill="' + C.offline + '"/>' +
           '<text x="188" y="4" font-size="' + rsp.legendFS + '" fill="' + C.textMuted + '" font-weight="500">离线用户</text>';
    svg += '</g>';

    // === server node (机柜式) ===
    // 机柜宽度略小于直径的 2r,留出圆角空间
    var cabW = Math.round(serverR * 1.55);
    var cabH = Math.round(serverR * 1.55);
    var cabX = serverX - Math.round(cabW / 2);
    var cabY = serverY - Math.round(cabH / 2);
    var cabR = Math.round(serverR * 0.22);   // 圆角半径

    // === connection lines ===
    // 静态虚线 (底)— 接服务器矩形底边中央
    var linkStartY = cabY + cabH;
    for (var i = 0; i < n; i++) {
      var ux = n === 1 ? serverX : startX + (endX - startX) * (i / (n - 1));
      var isOn = !!visible[i].online;
      svg += '<line class="topology-link" x1="' + serverX + '" y1="' + linkStartY +
             '" x2="' + ux + '" y2="' + (userY - userR) +
             '" stroke="' + (isOn ? C.linkOnline : C.link) +
             '" stroke-width="' + (isOn ? '1.4' : '1') +
             '" stroke-dasharray="' + C.linkDash +
             '" opacity="' + (isOn ? '0.85' : '0.45') + '"/>';
    }

    // === server node (机柜式) ===
    var ledColor = serverState === 'ok' ? C.online : (serverState === 'warn' ? '#fbbf24' : C.offline);
    var ledGlowId = 'led-glow-' + uid;

    // 外圈呼吸 halo(保持原视觉)
    svg += '<rect x="' + (cabX - 14) + '" y="' + (cabY - 14) +
           '" width="' + (cabW + 28) + '" height="' + (cabH + 28) +
           '" rx="' + (cabR + 12) + '" ry="' + (cabR + 12) +
           '" fill="url(#server-halo-' + uid + ')" class="topology-halo-pulse"/>';

    // 机柜主体(深紫渐变,带玻璃感)
    svg += '<g class="topology-node-server" data-name="server">';
    svg += '<rect x="' + cabX + '" y="' + cabY + '" width="' + cabW + '" height="' + cabH +
           '" rx="' + cabR + '" ry="' + cabR +
           '" fill="url(#server-body-' + uid + ')" filter="url(#node-glow-' + uid + ')"/>';
    // 机柜高光描边
    svg += '<rect x="' + cabX + '" y="' + cabY + '" width="' + cabW + '" height="' + cabH +
           '" rx="' + cabR + '" ry="' + cabR +
           '" fill="none" stroke="rgba(255,255,255,0.35)" stroke-width="1"/>';
    // 顶部 LED 横条区
    var ledAreaH = Math.max(6, Math.round(cabH * 0.18));
    var ledY = cabY + Math.round(cabH * 0.10);
    svg += '<rect x="' + (cabX + Math.round(cabW * 0.12)) +
           '" y="' + ledY +
           '" width="' + Math.round(cabW * 0.76) +
           '" height="' + ledAreaH +
           '" rx="' + Math.round(ledAreaH / 2) +
           '" fill="rgba(0,0,0,0.30)"/>';
    // 3 颗 LED
    var ledGap = Math.round(cabW * 0.10);
    var ledW = Math.round((cabW * 0.76 - ledGap * 2) / 3);
    var ledStates = serverState === 'ok' ? [true, true, true]
                  : serverState === 'warn' ? [true, true, false]
                  : [false, false, false];
    var ledColors = serverState === 'ok' ? [ledColor, ledColor, ledColor]
                  : serverState === 'warn' ? [ledColor, '#fbbf24', '#fbbf24']
                  : [C.offline, C.offline, C.offline];
    for (var li = 0; li < 3; li++) {
      var lx = cabX + Math.round(cabW * 0.12) + li * (ledW + ledGap);
      var ly = ledY;
      svg += '<rect x="' + lx + '" y="' + ly + '" width="' + ledW + '" height="' + ledAreaH +
             '" rx="' + Math.round(ledAreaH / 2) +
             '" fill="' + ledColors[li] + '" opacity="' + (ledStates[li] ? '1' : '0.25') + '" filter="' +
             (ledStates[li] ? 'url(#' + ledGlowId + ')' : '') +
             '" class="' + (ledStates[li] ? 'topology-led-on' : 'topology-led-off') + '"/>';
    }
    // 中央屏幕(显示柱状图,表示流量活跃度)
    var scrX = cabX + Math.round(cabW * 0.16);
    var scrY = cabY + Math.round(cabH * 0.42);
    var scrW = cabW - Math.round(cabW * 0.32);
    var scrH = Math.round(cabH * 0.32);
    svg += '<rect x="' + scrX + '" y="' + scrY + '" width="' + scrW + '" height="' + scrH +
           '" rx="' + Math.round(scrH * 0.18) +
           '" fill="rgba(8,14,32,0.85)" stroke="rgba(34,211,238,0.30)" stroke-width="1"/>';
    // 柱状图(5 条 bar,中央最高,active 时随机波动)
    var barCount = 5;
    var barGap = Math.round(scrW * 0.06);
    var barW = Math.round((scrW - barGap * (barCount - 1) - scrW * 0.10) / barCount);
    var barAreaH = scrH - Math.round(scrH * 0.20);
    var heights = serverState === 'ok' ? [0.55, 0.85, 1.0, 0.75, 0.45]
                : serverState === 'warn' ? [0.35, 0.55, 0.40, 0.30, 0.25]
                : [0.18, 0.22, 0.20, 0.16, 0.15];
    for (var bi = 0; bi < barCount; bi++) {
      var bx = scrX + Math.round(scrW * 0.05) + bi * (barW + barGap);
      var bh = Math.round(barAreaH * heights[bi]);
      var by = scrY + scrH - Math.round(scrH * 0.10) - bh;
      var bColor = serverState === 'ok' ? C.online
                 : serverState === 'warn' ? '#fbbf24'
                 : C.offline;
      var animClass = (serverState === 'ok' && bi === 2) ? ' class="topology-bar-pulse"' : '';
      svg += '<rect x="' + bx + '" y="' + by + '" width="' + barW + '" height="' + bh +
             '" rx="' + Math.max(1, Math.round(barW * 0.35)) +
             '" fill="' + bColor + '" opacity="' + (serverState === 'ok' ? '0.95' : '0.70') + '"' + animClass + '/>';
    }
    // 底部通风槽(3 条横向凹槽)
    var ventCount = 3;
    var ventH = Math.max(1, Math.round(cabH * 0.025));
    var ventW = cabW - Math.round(cabW * 0.30);
    var ventX = cabX + Math.round(cabW * 0.15);
    var ventStartY = cabY + cabH - Math.round(cabH * 0.16);
    for (var vi = 0; vi < ventCount; vi++) {
      var vyy = ventStartY + vi * (ventH + Math.max(2, Math.round(ventH * 1.4)));
      svg += '<rect x="' + ventX + '" y="' + vyy + '" width="' + ventW + '" height="' + ventH +
             '" rx="' + Math.max(1, Math.round(ventH / 2)) +
             '" fill="rgba(0,0,0,0.45)"/>';
    }
    svg += '</g>';

    // 装饰旋转环(改放外层,围绕机柜)
    svg += '<circle cx="' + serverX + '" cy="' + serverY + '" r="' + (Math.round(Math.max(cabW, cabH) / 2) + 6) +
           '" fill="none" stroke="' + C.serverRing + '" stroke-width="1" stroke-dasharray="2 4" class="topology-ring-spin"/>';
    // 标签
    svg += '<text x="' + serverX + '" y="' + (serverY + serverR + 22) +
           '" text-anchor="middle" fill="' + C.textPrimary + '" font-size="' + rsp.labelFS +
           '" font-weight="600">IKEv2 Server</text>';
    svg += '<text x="' + serverX + '" y="' + (serverY + serverR + 38) +
           '" text-anchor="middle" fill="' + C.textMono + '" font-family="ui-monospace, Menlo, monospace" font-size="' + rsp.subFS + '">' +
           escapeHTML(serverAddr || '— 未配置 —') + '</text>';

    // === user nodes ===
    for (var j = 0; j < n; j++) {
      var cx = n === 1 ? serverX : startX + (endX - startX) * (j / (n - 1));
      var cy = userY;
      var cli = visible[j];
      var isOnline = !!cli.online;
      var isFocus = cli.name === focusUser;
      var nodeColor = isOnline ? C.online : C.offline;
      var nodeHaloId = isOnline ? 'online-halo-' + uid : null;

      // focus 强调环(叠加 cyan)
      if (isFocus && isOnline) {
        svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + (userR + 10) +
               '" fill="none" stroke="' + C.focusGlow + '" stroke-width="2" class="topology-focus-ring"/>';
      }

      // 用户节点 halo
      if (nodeHaloId) {
        svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + (userR + 6) +
               '" fill="url(#' + nodeHaloId + ')" class="topology-halo-pulse"/>';
      }

      var nodeClass = 'topology-node-user' + (isOnline ? ' is-online' : ' is-offline') + (isFocus ? ' is-focus' : '');
      svg += '<g class="' + nodeClass + '" data-name="' + escapeHTML(cli.name) + '">';
      svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + userR +
             '" fill="' + nodeColor + '" filter="url(#node-glow-' + uid + ')" opacity="' + (isOnline ? '1' : '0.6') + '"/>';
      svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + (userR - 3) +
             '" fill="none" stroke="rgba(255,255,255,' + (isOnline ? '0.45' : '0.20') + '" stroke-width="1"/>';

      // 内部符号:online 实心圆点 / offline 空心
      if (isOnline) {
        svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + Math.max(3, Math.round(userR * 0.32)) +
               '" fill="#fff" class="topology-pulse-dot"/>';
      } else {
        svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + Math.max(3, Math.round(userR * 0.30)) +
               '" fill="none" stroke="rgba(255,255,255,0.6)" stroke-width="1.5"/>';
      }
      svg += '</g>';

      // 名字
      svg += '<text x="' + cx + '" y="' + (cy + userR + 18) +
             '" text-anchor="middle" fill="' + (isFocus ? C.onlineHi : C.textPrimary) +
             '" font-size="' + rsp.labelFS + '" font-weight="' + (isFocus ? '700' : '600') + '">' +
             escapeHTML(cli.name) + '</text>';

      // 流量 / 状态
      var sub;
      if (isOnline && (cli.bytesIn || cli.bytesOut)) {
        sub = '↓ ' + formatBytes(cli.bytesIn) + '   ↑ ' + formatBytes(cli.bytesOut);
      } else if (isOnline) {
        sub = '在线 · 待机';
      } else {
        sub = '离线';
      }
      svg += '<text x="' + cx + '" y="' + (cy + userR + 33) +
             '" text-anchor="middle" fill="' + (isOnline ? C.textMono : C.textMuted) +
             '" font-size="' + rsp.subFS + '" font-family="ui-monospace, Menlo, monospace">' +
             sub + '</text>';

      // 流动光点(仅在线,沿连线下行 server→user & 上行 user→server)
      if (isOnline) {
        var linkLen = Math.round(Math.hypot(serverX - cx, linkStartY - (userY - userR)));
        svg += '<circle class="topology-packet topology-packet-down" data-linklen="' + linkLen + '" r="3" fill="' + C.flowDown + '">';
        svg += '<animateMotion dur="' + (2.4 + j * 0.18).toFixed(2) +
               's" repeatCount="indefinite" rotate="auto" path="M ' + cx + ' ' + (userY - userR) +
               ' L ' + serverX + ' ' + linkStartY + '"/>';
        svg += '</circle>';
        svg += '<circle class="topology-packet topology-packet-up" data-linklen="' + linkLen + '" r="2.5" fill="' + C.flowUp + '" opacity="0.85">';
        svg += '<animateMotion dur="' + (3.2 + j * 0.22).toFixed(2) +
               's" repeatCount="indefinite" path="M ' + serverX + ' ' + linkStartY +
               ' L ' + cx + ' ' + (userY - userR) + '"/>';
        svg += '</circle>';
      }

      // hitbox (放大点击区,放在最上层)
      var href = cli.href || ('/users/' + (cli.id || ''));
      svg += '<a href="' + escapeHTML(href) + '" class="topology-hitbox" aria-label="查看 ' + escapeHTML(cli.name) + '">';
      svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + (userR + 8) + '" fill="transparent"/>';
      svg += '</a>';
    }

    // 超出截断提示
    if (clients.length > maxClients) {
      svg += '<text x="' + (W / 2) + '" y="' + (userY + userR + 52) +
             '" text-anchor="middle" fill="' + C.textMuted + '" font-size="' + rsp.subFS + '">+' +
             (clients.length - maxClients) + ' 更多用户未在图中展示</text>';
    }

    svg += '</svg>';

    root.innerHTML = svg;

    // === inject stylesheet (keyframes & hover) ===
    var styleId = 'topology-flow-style';
    if (!document.getElementById(styleId)) {
      var s = document.createElement('style');
      s.id = styleId;
      s.textContent =
        // 节点入场缩放
        '.topology-svg .topology-node-server, .topology-svg .topology-node-user {' +
          'transform-origin:center;' +
          'transform-box:fill-box;' +
          'animation: topo-node-in 480ms cubic-bezier(.2,.7,.3,1.2) both;' +
        '}' +
        '.topology-svg .topology-node-user{ animation-delay:80ms;}' +
        '@keyframes topo-node-in{' +
          'from{ opacity:0; transform:scale(.55);}' +
          'to  { opacity:1; transform:scale(1);}' +
        '}' +
        // 节点呼吸 halo
        '.topology-svg .topology-halo-pulse{' +
          'transform-origin:center; transform-box:fill-box;' +
          'animation: topo-halo-pulse 2.6s ease-in-out infinite;' +
        '}' +
        '@keyframes topo-halo-pulse{' +
          '0%,100%{ opacity:.55; transform:scale(1);}' +
          '50%    { opacity:.95; transform:scale(1.12);}' +
        '}' +
        // 装饰环旋转
        '.topology-svg .topology-ring-spin{' +
          'transform-origin:center; transform-box:fill-box;' +
          'animation: topo-ring-spin 14s linear infinite;' +
        '}' +
        '@keyframes topo-ring-spin{' +
          'to{ transform:rotate(360deg);}' +
        '}' +
        // 内部小白点闪烁
        '.topology-svg .topology-pulse-dot{' +
          'transform-origin:center; transform-box:fill-box;' +
          'animation: topo-pulse-dot 1.6s ease-in-out infinite;' +
        '}' +
        '@keyframes topo-pulse-dot{' +
          '0%,100%{ opacity:.95; transform:scale(1);}' +
          '50%    { opacity:.55; transform:scale(.7);}' +
        '}' +
        // focus 强调环呼吸
        '.topology-svg .topology-focus-ring{' +
          'transform-origin:center; transform-box:fill-box;' +
          'animation: topo-focus-ring 1.8s ease-in-out infinite;' +
        '}' +
        '@keyframes topo-focus-ring{' +
          '0%,100%{ opacity:.6;  transform:scale(1);}' +
          '50%    { opacity:1;   transform:scale(1.12);}' +
        '}' +
        // server LED 闪烁
        '.topology-svg .topology-led-on{' +
          'animation: topo-led-blink 1.8s ease-in-out infinite;' +
        '}' +
        '@keyframes topo-led-blink{' +
          '0%,100%{ opacity:1;}' +
          '45%    { opacity:.55;}' +
          '55%    { opacity:1;}' +
        '}' +
        // server 屏幕柱状图中央跳动
        '.topology-svg .topology-bar-pulse{' +
          'transform-origin:bottom center; transform-box:fill-box;' +
          'animation: topo-bar-pulse 1.2s ease-in-out infinite;' +
        '}' +
        '@keyframes topo-bar-pulse{' +
          '0%,100%{ transform:scaleY(1);}' +
          '50%    { transform:scaleY(.55);}' +
        '}' +
        // hitbox hover
        '.topology-svg .topology-hitbox:hover + circle,' +
        '.topology-svg g.topology-node-user:hover + .topology-link,' +
        '.topology-svg .topology-hitbox:hover{' +
          'cursor:pointer;' +
        '}' +
        // prefers-reduced-motion 关闭动画
        '@media (prefers-reduced-motion: reduce){' +
          '.topology-svg .topology-node-server, .topology-svg .topology-node-user,' +
          '.topology-svg .topology-halo-pulse, .topology-svg .topology-ring-spin,' +
          '.topology-svg .topology-pulse-dot, .topology-svg .topology-focus-ring,' +
          '.topology-svg .topology-led-on, .topology-svg .topology-bar-pulse{' +
            'animation:none !important;' +
          '}' +
        '}';
      document.head.appendChild(s);
    }
  }

  function init() {
    var nodes = document.querySelectorAll('.topology[data-clients]');
    for (var i = 0; i < nodes.length; i++) renderTopology(nodes[i]);
  }

  // 暴露给外部调用以重渲染(在用户状态变更后)
  window.renderTopology = renderTopology;

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
