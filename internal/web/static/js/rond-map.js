/* rond 足迹地图：底图坐标系感知 + Leaflet 渲染
 *
 * 坐标系约定：数据库里的地点坐标是 GCJ-02（火星坐标，见 internal/geo/geo.go 的说明），
 * 因此画在 GCJ-02 底图（高德/腾讯）上时**不做任何转换**；
 * 只有换成 WGS-84 底图（天地图 / OSM / 世界迷雾）时才反算一次，否则会偏 500 米以上。
 */
(function () {
  'use strict';

  var PI = 3.1415926535897932384626, A = 6378245.0, EE = 0.00669342162296594323;
  var DATA_CRS = 'gcj02';

  // 交通方式图例：轨迹开启时告诉用户颜色含义
  function modeLegendControl(modes) {
    var Ctl = L.Control.extend({
      options: { position: 'bottomleft' },
      onAdd: function () {
        var box = L.DomUtil.create('div', 'mode-legend');
        box.style.cssText = 'background:rgba(255,255,255,.92);padding:6px 9px;border-radius:8px;' +
          'box-shadow:0 1px 4px rgba(0,0,0,.18);font-size:11.5px;line-height:1.7';
        modes.forEach(function (m) {
          var row = L.DomUtil.create('div');
          row.innerHTML = '<span style="display:inline-block;width:14px;height:3px;vertical-align:middle;' +
            'background:' + (m.color || '#30b0c7') + ';margin-right:6px"></span>' +
            m.name + ' <span style="color:#8a9199">' + m.count + '</span>';
          box.appendChild(row);
        });
        L.DomEvent.disableClickPropagation(box);
        return box;
      }
    });
    return new Ctl();
  }

  // GPX 轨迹按需加载：勾选时才请求，切底图后按新坐标系重取
  var gpxCache = {};
  function ensureGpx(crs, done) {
    if (!gpxCache[crs]) {
      gpxCache[crs] = fetch('/api/gpx?crs=' + crs)
        .then(function (r) { return r.json(); })
        .then(function (d) { return d.tracks || []; })
        .catch(function () { delete gpxCache[crs]; return []; });
    }
    gpxCache[crs].then(done);
  }

  // 按需加载 leaflet-heat：同一个 promise 复用，避免连点重复插入脚本
  var heatLoading = null;
  function ensureHeat(done) {
    if (L.heatLayer) { done(); return; }
    if (!heatLoading) {
      heatLoading = new Promise(function (resolve, reject) {
        var s = document.createElement('script');
        s.src = '/static/vendor/heat/leaflet-heat.js' + (window.__assetV ? '?v=' + window.__assetV : '');
        s.onload = resolve;
        s.onerror = reject;
        document.head.appendChild(s);
      });
    }
    heatLoading.then(done, function () { heatLoading = null; });
  }

  // 迷雾浓度滑杆：不同底图明暗差异大，让用户自己调节更直观。
  // 返回控件本身，便于迷雾被关掉时把滑杆一并收起来——图层关着、滑杆还在，
  // 会让人以为迷雾已经打开了。
  function addFogOpacityControl(map, layer, corner) {
    var Ctl = L.Control.extend({
      options: { position: corner },
      onAdd: function () {
        var box = L.DomUtil.create('div', 'leaflet-bar fog-opacity');
        // 标签做成按钮：窄屏用它把滑杆收起来（右下角控件太多时太挤）
        box.innerHTML = '<button type="button" class="fog-label">迷雾浓度</button>';
        var label = box.querySelector('.fog-label');
        var slider = L.DomUtil.create('input', 'fog-slider');
        slider.type = 'range';
        slider.min = '20';
        slider.max = '100';
        slider.step = '5';
        slider.value = String(Math.round(layer.options.opacity * 100));
        box.appendChild(slider);
        L.DomEvent.disableClickPropagation(box);
        L.DomEvent.disableScrollPropagation(box);
        L.DomEvent.on(label, 'click', function () { box.classList.toggle('open'); });
        L.DomEvent.on(slider, 'input', function () {
          var v = parseInt(slider.value, 10) / 100;
          layer.setOpacity(v);
          localStorage.setItem('rond.fogOpacity', String(v));
        });
        return box;
      }
    });
    return new Ctl().addTo(map);
  }

  function inChina(lat, lon) {
    return lon >= 73.66 && lon <= 135.05 && lat >= 3.86 && lat <= 53.55;
  }

  function transformLat(x, y) {
    var r = -100 + 2 * x + 3 * y + 0.2 * y * y + 0.1 * x * y + 0.2 * Math.sqrt(Math.abs(x));
    r += (20 * Math.sin(6 * x * PI) + 20 * Math.sin(2 * x * PI)) * 2 / 3;
    r += (20 * Math.sin(y * PI) + 40 * Math.sin(y / 3 * PI)) * 2 / 3;
    r += (160 * Math.sin(y / 12 * PI) + 320 * Math.sin(y * PI / 30)) * 2 / 3;
    return r;
  }

  function transformLon(x, y) {
    var r = 300 + x + 2 * y + 0.1 * x * x + 0.1 * x * y + 0.1 * Math.sqrt(Math.abs(x));
    r += (20 * Math.sin(6 * x * PI) + 20 * Math.sin(2 * x * PI)) * 2 / 3;
    r += (20 * Math.sin(x * PI) + 40 * Math.sin(x / 3 * PI)) * 2 / 3;
    r += (150 * Math.sin(x / 12 * PI) + 300 * Math.sin(x / 30 * PI)) * 2 / 3;
    return r;
  }

  // WGS-84 -> GCJ-02
  function wgsToGcj(lat, lon) {
    if (!inChina(lat, lon)) return [lat, lon];
    var dLat = transformLat(lon - 105, lat - 35);
    var dLon = transformLon(lon - 105, lat - 35);
    var radLat = lat / 180 * PI;
    var magic = Math.sin(radLat);
    magic = 1 - EE * magic * magic;
    var sqrtMagic = Math.sqrt(magic);
    dLat = (dLat * 180) / ((A * (1 - EE)) / (magic * sqrtMagic) * PI);
    dLon = (dLon * 180) / (A / sqrtMagic * Math.cos(radLat) * PI);
    return [lat + dLat, lon + dLon];
  }

  // GCJ-02 -> WGS-84，正变换没有解析反函数，不动点迭代即可
  function gcjToWgs(lat, lon) {
    if (!inChina(lat, lon)) return [lat, lon];
    var wLat = lat, wLon = lon;
    for (var i = 0; i < 5; i++) {
      var g = wgsToGcj(wLat, wLon);
      wLat += lat - g[0];
      wLon += lon - g[1];
    }
    return [wLat, wLon];
  }

  function project(lat, lon, crs) {
    if (crs === 'wgs84') return gcjToWgs(lat, lon);
    return [lat, lon];
  }

  var APPLE = {
    red: '#ff3b30', orange: '#ff9500', yellow: '#e8b700', green: '#34c759', mint: '#00c7be',
    teal: '#30b0c7', cyan: '#32ade6', blue: '#007aff', indigo: '#5856d6', purple: '#af52de',
    pink: '#ff2d55', brown: '#a2845e', gray: '#8e8e93'
  };

  var maps = {};
  var cfgCache = null;

  function register(id, map) {
    maps[id] = map;
    window.__rondMaps = maps;
    return map;
  }

  function colorOf(name) {
    if (!name) return '#5b6169';
    if (APPLE[name]) return APPLE[name];
    return /^#/.test(name) ? name : '#5b6169';
  }

  // 底图配置由服务端下发：需要密钥的图层走 /tiles/... 代理，密钥不进页面
  function loadConfig() {
    if (cfgCache) return Promise.resolve(cfgCache);
    return fetch('/api/map-config', { headers: { 'Accept': 'application/json' } })
      .then(function (r) { return r.json(); })
      .then(function (c) {
        if (!c || !c.base || !c.base.length) throw new Error('底图配置为空');
        cfgCache = c;
        return c;
      })
      .catch(function () {
        // 兜底：直接用免密钥高德瓦片，保证地图不会空白
        cfgCache = {
          base: [{
            key: 'amap-street', label: '街道', crs: 'gcj02', maxZoom: 18,
            url: 'https://webrd0{s}.is.autonavi.com/appmaptile?lang=zh_cn&size=1&scale=1&style=8&x={x}&y={y}&z={z}',
            attr: '© 高德地图'
          }],
          overlays: []
        };
        return cfgCache;
      });
  }

  function tileLayer(def) {
    return L.tileLayer(def.url, {
      subdomains: ['1', '2', '3', '4'],
      maxZoom: def.maxZoom || 18,
      minZoom: 3,
      attribution: def.attr || ''
    });
  }

  /* ---------- 默认底图 ----------
   * 三处来源，优先级从高到低：
   *   1) 访客自己在图上切过的那张（localStorage，换设备就没了，但不用每次重切）
   *   2) 后台「设置 → 地图底图 → 默认用哪张底图」设的那张
   *   3) 配置里的第一张
   * Key 对不上（换过 map_provider、旧配置残留）就往下落，绝不让地图空着。
   */
  var BASE_PICK = 'rond.base';

  function pickBase(cfg) {
    var byKey = {};
    cfg.base.forEach(function (b) { byKey[b.key] = b; });
    try {
      var saved = window.localStorage.getItem(BASE_PICK);
      if (saved && byKey[saved]) return byKey[saved];
    } catch (e) { /* 隐私模式禁用了 localStorage，忽略即可 */ }
    if (cfg.default && byKey[cfg.default]) return byKey[cfg.default];
    return cfg.base[0];
  }

  function rememberBase(key) {
    if (!key) return;
    try { window.localStorage.setItem(BASE_PICK, key); } catch (e) { /* 同上 */ }
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function dur(min) {
    min = Math.round(min || 0);
    if (min <= 0) return '—';
    var d = Math.floor(min / 1440), h = Math.floor((min % 1440) / 60), m = min % 60;
    if (d > 0) return d + '天' + (h ? h + '小时' : '');
    if (h > 0) return h + '小时' + (m ? m + '分' : '');
    return m + '分';
  }

  /* ---------- 推荐 / 踩雷：地图气泡里直接表决 ----------
   * 票数一次性从 /api/votes 取回（个人站点规模很小），气泡渲染时同步查表，
   * 不为每个点单独发请求；投票就地 AJAX，不刷新页面。
   */
  var voteData = null;                     // {verdicts:{}, tallies:{}, mine:{}}
  var voteMeta = { admin: false, public: false };
  var voteLoad = null;

  function loadVotes() {
    if (voteLoad) return voteLoad;
    voteLoad = fetch('/api/votes', { headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        voteMeta.admin = !!d.admin;
        voteMeta.public = !!d.public;
        voteData = { verdicts: d.verdicts || {}, tallies: d.tallies || {}, mine: d.mine || {} };
        repaintVotes();
        return voteData;
      })
      .catch(function () { voteLoad = null; return null; });
    return voteLoad;
  }

  function verdictOf(pk, fallback) {
    if (voteData && voteData.verdicts[pk] != null) return voteData.verdicts[pk];
    return fallback || 0;
  }

  function tallyOf(pk, up, down) {
    var t = voteData && voteData.tallies[pk];
    return t || { up: up || 0, down: down || 0 };
  }

  function paintVote(box, p) {
    var pk = p.sp;
    // 票数与身份（站长 / 访客）还没拿到时先不画，免得先闪出「大家怎么说」再变成「我的结论」
    if (!voteData) { box.style.display = 'none'; return; }
    if (!(voteMeta.admin || voteMeta.public)) { box.style.display = 'none'; return; }
    box.style.display = '';
    // 管理员看自己的结论（点已选中的按钮可取消）；访客看大家怎么说
    if (voteMeta.admin) {
      var v = verdictOf(pk, p.vd);
      box.innerHTML = '<span class="mp-vl">我的结论</span>' +
        '<button type="button" class="mp-vb up' + (v === 1 ? ' on' : '') + '" data-set="' + (v === 1 ? 0 : 1) + '">👍 推荐</button>' +
        '<button type="button" class="mp-vb down' + (v === -1 ? ' on' : '') + '" data-set="' + (v === -1 ? 0 : -1) + '">👎 踩雷</button>';
    } else {
      var t = tallyOf(pk, p.u, p.dn), m = mineArea(pk);
      box.innerHTML = '<span class="mp-vl">大家怎么说</span>' +
        '<button type="button" class="mp-vb up' + (m === 1 ? ' on' : '') + '" data-set="1">👍 <b>' + t.up + '</b></button>' +
        '<button type="button" class="mp-vb down' + (m === -1 ? ' on' : '') + '" data-set="-1">👎 <b>' + t.down + '</b></button>';
    }
    Array.prototype.forEach.call(box.querySelectorAll('.mp-vb'), function (b) {
      b.addEventListener('click', function () {
        if (b.disabled) return;
        b.disabled = true;
        var body = voteMeta.admin
          ? 'src_pk=' + pk + '&verdict=' + b.getAttribute('data-set')
          : 'src_pk=' + pk + '&vote=' + b.getAttribute('data-set');
        fetch(voteMeta.admin ? '/api/verdict' : '/api/vote', {
          method: 'POST',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          credentials: 'same-origin',
          body: body
        })
          .then(function (r) { return r.json(); })
          .then(function (d) {
            if (d.error) { b.disabled = false; return; }
            if (!voteData) voteData = { verdicts: {}, tallies: {}, mine: {} };
            if (voteMeta.admin) {
              if (d.verdict) voteData.verdicts[pk] = d.verdict; else delete voteData.verdicts[pk];
            } else {
              voteData.tallies[pk] = { up: d.up, down: d.down };
              if (d.mine) voteData.mine[pk] = d.mine; else delete voteData.mine[pk];
            }
            paintVote(box, p);
          })
          .catch(function () { b.disabled = false; });
      });
    });
  }

  function mineArea(pk) {
    return voteData && voteData.mine[pk] ? voteData.mine[pk] : 0;
  }

  // 票数异步到货后，把已经打开的气泡补上真实票数
  function repaintVotes() {
    Array.prototype.forEach.call(document.querySelectorAll('.mp-vote'), function (box) {
      if (box.__p) paintVote(box, box.__p);
    });
  }

  function popup(p, vote) {
    var wrap = document.createElement('div');
    wrap.className = 'mp';
    var parts = ['<div class="mp-n">' + esc(p.n || '未命名地点') + '</div>'];
    var meta = [];
    if (p.a) meta.push(esc(p.a));
    if (p.c) meta.push(esc(p.c));
    if (meta.length) parts.push('<div class="mp-m">' + meta.join(' · ') + '</div>');
    parts.push('<div class="mp-m">到访 <b>' + p.v + '</b> 次 · 停留 <b>' + dur(p.d) + '</b></div>');
    // 备注只读直出，省得多点一次详情页
    if (p.note) parts.push('<div class="mp-note">' + esc(p.note) + '</div>');
    if (p.t) parts.push('<div class="mp-m">最近一次 ' + esc(p.t) + '</div>');
    parts.push('<div class="mp-foot"><a href="/places/' + p.id + '">查看详情 →</a></div>');
    wrap.innerHTML = parts.join('');
    if (vote && p.sp) {
      // 无论如何都挂上，交给 paintVote 决定显不显示；
      // 不挂的话票数到货后的 repaintVotes() 就找不到它了
      var box = L.DomUtil.create('div', 'mp-vote');
      box.__p = p;
      paintVote(box, p);
      wrap.appendChild(box);
    }
    return wrap;
  }

  // radiusOf 让点的大小反映到访次数（按当前这批里最多的那个归一化）
  function radiusOf(p, max) {
    return 4 + Math.min(9, Math.sqrt(p.v / max) * 9);
  }

  function maxVisit(points) {
    var max = 1;
    points.forEach(function (p) { if (p.v > max) max = p.v; });
    return max;
  }

  // makeMarker 造一个点。抽出来是为了让 setPoints 能只补差集：
  // 整体重建会让所有点同时闪一下，聚合的展开状态也会丢。
  function makeMarker(p, crs, max, vote) {
    var c = project(p.lat, p.lon, crs);
    var m = L.circleMarker([c[0], c[1]], baseStyle(p, max));
    // 传函数而不是字符串：每次打开都重算票数与「我投的」，避免陈旧的闭包数据
    // maxWidth 要比默认的 300 大，否则「我的结论 👍 推荐 👎 踩雷」会被挤成两行
    m.bindPopup(function () { return popup(p, vote); },
      { closeButton: true, maxWidth: 360, minWidth: 240 });
    m.bindTooltip(p.n || '未命名', { direction: 'top', offset: [0, -4] });
    m.__p = p; // 记住这份数据是谁的，用来判断下批是不是「重新拉的」
    return m;
  }

  function makeGroup(useCluster) {
    return useCluster && L.markerClusterGroup
      ? L.markerClusterGroup({ maxClusterRadius: 46, disableClusteringAtZoom: 15, showCoverageOnHover: false })
      : L.layerGroup();
  }

  // index 可选：传了就顺手填好「点 id → 图层」，供 setPoints 做增量更新，
  // 也供 highlight() 按 key 找到某个点。没有 id 的点用调用方给的 p.key 当键
  // （照片导入页传的是行号，它本来就没有库内 id）。
  function markers(points, crs, useCluster, vote, index) {
    var group = makeGroup(useCluster);
    var max = maxVisit(points);
    points.forEach(function (p) {
      var m = makeMarker(p, crs, max, vote);
      if (index) index[p.id !== undefined ? p.id : p.key] = m;
      group.addLayer(m);
    });
    return group;
  }

  // 点的「常态样式」：highlight 复原时要用同一套值，所以抽出来
  function baseStyle(p, max) {
    return {
      radius: radiusOf(p, max), color: '#fff', weight: 1.5,
      fillColor: colorOf(p.k), fillOpacity: 0.9
    };
  }

  function reducedMotion() {
    return !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
  }

  // popIn 给一个元素打上入场动画。动画结束后把类和延时清掉，
  // 免得这个元素之后被复用（缩放时 Leaflet 会换元素）时莫名其妙又跑一遍。
  function popIn(el, delay) {
    if (!el || !el.classList) return;
    el.style.animationDelay = delay;
    el.classList.add('pt-new');
    el.addEventListener('animationend', function () {
      el.classList.remove('pt-new');
      el.style.animationDelay = '';
    }, { once: true });
  }

  // 新出现的点做一次「冒出」动画，一批按顺序错开一点，看起来是"扫过来"的。
  //
  // 必须等图层真正上屏再打类：markerClusterGroup 是**异步**把子标记渲染成 DOM 的，
  // addLayer 之后立刻 getElement() 拿到的是 null（这也是第一版动画完全没生效的原因）。
  // 所以挂 add 事件，并且顺手判一次「是不是已经上屏了」。
  function animateIn(markers) {
    if (reducedMotion()) return;
    markers.forEach(function (m, i) {
      var delay = Math.min(i, 24) * 10 + 'ms';
      var done = false;
      var fire = function () {
        if (done) return;
        done = true;
        popIn(m.getElement && m.getElement(), delay);
      };
      if (m.once) m.once('add', fire);
      fire();
    });
  }

  // 聚合图标不能用 DOM 元素本身当身份：markerCluster 每次数据变动都会重建它们
  // （实测一轮更新里往 marker pane 加了 791 个元素），拿元素当 key 每次都被当成
  // 「第一次见到」，结果就是什么都不会触发。用它的**位置**当身份才稳。
  function clusterKey(el) {
    var t = el.style.transform || '';
    var m = t.match(/-?\d+(?:\.\d+)?/g);
    if (m && m.length >= 2) {
      // 位置量化到 40px 一格：点集一变，聚合中心会跟着微微挪动，
      // 不量化的话同一个圈每次都被当成「新出现的」，回放时一片乱闪。
      return Math.round(Number(m[0]) / 40) + ',' + Math.round(Number(m[1]) / 40);
    }
    return el.style.left + ',' + el.style.top;
  }

  // clusterState 把当前所有聚合圈的位置 → 数字记下来，并在数字变化 /
  // 出现新位置时给相应元素打动画类。返回给下一次调用当基线。
  function clusterState(map, prev) {
    var now = {};
    var changed = [];
    var appeared = [];
    [].slice.call(map.getContainer().querySelectorAll('.marker-cluster')).forEach(function (el) {
      var inner = el.firstElementChild;
      if (!inner) return;
      var key = clusterKey(el);
      var n = (inner.textContent || '').trim();
      now[key] = n;
      if (!prev) return; // 第一次只记基线：别一上来满屏都在动
      if (prev[key] === undefined) appeared.push(el);
      else if (prev[key] !== n) changed.push(inner);
    });
    if (!prev || reducedMotion()) return now;
    appeared.forEach(function (el, i) { popIn(el, Math.min(i, 24) * 10 + 'ms'); });
    // 数字变了轻弹一下。回放时在聚合视图下主要看到的就是数字在跳，
    // 光靠新圆圈淡入几乎看不出来。弹内层 div —— 外层带着 Leaflet 的定位 transform。
    changed.forEach(function (inner, i) {
      setTimeout(function () {
        inner.classList.remove('pt-pulse');
        void inner.offsetWidth; // 强制回流：连着两次同样的动画不加这句不会重放
        inner.classList.add('pt-pulse');
        inner.addEventListener('animationend', function () {
          inner.classList.remove('pt-pulse');
        }, { once: true });
      }, Math.min(i, 24) * 10);
    });
    return now;
  }

  function fit(map, points, crs, pad) {
    var lls = points.map(function (p) {
      var c = project(p.lat, p.lon, crs);
      return [c[0], c[1]];
    }).filter(function (c) { return c[0] || c[1]; });
    if (!lls.length) {
      map.setView([26.6, 106.7], 11);
      return;
    }
    map.fitBounds(L.latLngBounds(lls).pad(pad == null ? 0.15 : pad));
  }

  // 底图坐标系可能随图层切换而改变（例如高德 -> 天地图），此时必须重画标记
  function state4(map, points, group, crs, vote) {
    return {
      map: map, points: points, group: group, crs: crs, vote: vote,
      apply: function (nextCrs) {
        if (nextCrs === this.crs) return;
        this.map.removeLayer(this.group);
        this.crs = nextCrs;
        this.group = markers(this.points, this.crs, this.clustered, this.vote);
        this.group.addTo(this.map);
        fit(this.map, this.points, this.crs, this.pad);
      }
    };
  }

  window.rondMap = {
    gcj: wgsToGcj,
    gcjToWgs: gcjToWgs,
    colorOf: colorOf,
    esc: esc,
    dur: dur,

    // 简易预览：主页 / 地点详情用，无聚类、无控件。
    // 默认不带就地表决（详情页下方本来就有表决卡片，重复一套按钮反而混乱）；
    // 需要时传 vote: true。
    preview: function (opts) {
      var points = opts.points || [];
      var vote = opts.vote === true;
      // 同一个容器画第二次（照片导入页反查完画一次、点「重新反查」又画一次）：
      // Leaflet 不允许对已经有地图的容器再 L.map()，会抛
      // "Map container is already initialized."——那个错会被调用方的 catch 接住，
      // 于是「重新反查」明明成功了却报一句失败。这里直接复用那份实例换一批点。
      // 判据用「容器有没有地图」（register 记在 maps[] 里），不是「状态建全没有」：
      // 只要容器已被 Leaflet 认领过，再 L.map() 就一定会抛，哪怕上次的状态没建完。
      // 容器可能刚从 display:none 变回可见，先量一次尺寸（换点会顺手重贴视野）。
      var live = maps[opts.id];
      if (live) {
        live.invalidateSize();
        var prev = window.rondMap._state[opts.id];
        if (prev && prev.setPoints) prev.setPoints(points);
        else if (prev) prev.previewPending = points; // 配置还没回来，等 setPoints 建好再补画
        return;
      }
      var map = L.map(opts.id, { zoomControl: false, attributionControl: false, scrollWheelZoom: false });
      var st = null;
      register(opts.id, map);
      if (vote) loadVotes();
      loadConfig().then(function (cfg) {
        var def = pickBase(cfg);
        tileLayer(def).addTo(map);
        var byId = {};
        var group = markers(points, def.crs, false, vote, byId);
        group.addTo(map);
        var applyFit = function () { fit(map, points, def.crs, 0.06); };
        applyFit();
        // 点击缩略图进全屏地图：只有主页那种「预览」需要（opts.clickToFull）。
        // 地点详情页不能要——它上面本来就有「在地图中查看」按钮，点地图就走很突兀；
        // 后台照片页更不能要——点一下地图就被甩出后台。
        if (opts.clickToFull) {
          map.on('click', function () { window.location.href = '/map'; });
        }
        // 容器尺寸变化后重新贴合视野（卡片布局在字体加载后可能变高）
        window.addEventListener('resize', function () { map.invalidateSize(); applyFit(); });
        st = state4(map, points, group, def.crs, vote);
        st.clustered = false;
        st.pad = 0.06;
        st.byId = byId;
        st.max = maxVisit(points);
        st.setPoints = function (pts) {
          points = st.points = pts;
          if (st.hl) st.hl = null; // 旧图层马上要被丢掉，别留着悬空引用
          map.removeLayer(st.group);
          st.byId = {};
          st.max = maxVisit(pts);
          st.group = markers(pts, st.crs, false, vote, st.byId);
          st.group.addTo(map);
          applyFit();
        };
        window.rondMap._state[opts.id] = st;
        // 配置就绪前有别的调用挂上来的点，现在补画一次
        if (st.previewPending) {
          var queued = st.previewPending;
          st.previewPending = null;
          st.setPoints(queued);
        }
      });
    },

    // highlight(id, key)：把某个点临时挑出来（放大、换醒目色、浮出名字、压到最上层）。
    // key 传 null / undefined 复原。key 取点的 id（full() 那套），没有 id 时取 p.key
    // ——照片导入页传的是行号，拿它做「鼠标划到哪一行，图上哪个点跳出来」。
    // 视野里没有这个点时才把它平移进来：划过一行就动一次地图会很晃。
    highlight: function (id, key) {
      var st = window.rondMap._state[id];
      if (!st || !st.byId || !st.map) return;
      if (st.hl) {
        st.hl.setStyle(baseStyle(st.hl.__p || {}, st.max || 1));
        st.hl.closeTooltip();
        st.hl = null;
      }
      if (key === null || key === undefined) return;
      var m = st.byId[key];
      if (!m) return;
      m.setStyle({
        radius: radiusOf(m.__p || {}, st.max || 1) + 8,
        color: '#fff', weight: 3, fillColor: '#e8590c', fillOpacity: 1
      });
      m.bringToFront();
      m.openTooltip();
      var ll = m.getLatLng();
      if (!st.map.getBounds().pad(-0.12).contains(ll)) st.map.panTo(ll);
      st.hl = m;
    },

    // 全屏地图：聚类 + 图层切换 + 坐标纠偏
    full: function (opts) {
      var map = L.map(opts.id, { zoomControl: false, worldCopyJump: false, attributionControl: false });
      // 默认那个角标会显示「Leaflet | © 高德地图」，其中「Leaflet」是渲染库自己的链接，
      // 对访客没有任何意义，去掉（prefix: false 只去掉这一半）。
      // 底图版权（© 高德地图 / © OpenStreetMap）必须留着——那是上游的使用条款要求。
      L.control.attribution({ prefix: false, position: 'bottomright' }).addTo(map);
      var points = opts.points || [];
      // 窄屏筛选面板占满宽度，控件改挂右下角（该尺寸下图例已隐藏）
      var corner = window.matchMedia('(max-width: 620px)').matches ? 'bottomright' : 'topright';
      L.control.zoom({ position: corner }).addTo(map);
      L.control.scale({ imperial: false, position: 'bottomleft' }).addTo(map);

      register(opts.id, map);
      window.rondMap._state[opts.id] = { pending: true, points: points };
      loadVotes();

      loadConfig().then(function (cfg) {
        var bases = {}, def = pickBase(cfg);
        cfg.base.forEach(function (b) {
          var l = tileLayer(b);
          l.__crs = b.crs;
          l.__key = b.key;
          bases[b.label] = l;
        });
        var active = bases[def.label];
        active.addTo(map);

        // 瓦片加载兜底：当前底图短时间内连续加载失败就自动切到别的底图。
        // Leaflet 只是本地渲染库，失败的是上游瓦片源（高德等），这里把选择权自动接过来。
        var errCount = 0, errTimer = null;
        function noteFallback(msg) {
          var el = document.createElement('div');
          el.textContent = msg;
          el.style.cssText = 'position:absolute;left:50%;transform:translateX(-50%);top:10px;z-index:1000;' +
            'background:rgba(30,40,50,.85);color:#fff;padding:7px 14px;border-radius:8px;font-size:12.5px';
          map.getContainer().appendChild(el);
          setTimeout(function () { el.remove(); }, 6000);
        }
        Object.keys(bases).forEach(function (label) {
          bases[label].on('tileerror', function () {
            if (map.hasLayer(bases[label]) && bases[label] === active) {
              errCount++;
              clearTimeout(errTimer);
              errTimer = setTimeout(function () { errCount = 0; }, 10000);
              if (errCount >= 8) {
                errCount = 0;
                var others = Object.keys(bases).filter(function (l) { return l !== label; });
                if (!others.length) return;
                map.removeLayer(active);
                active = bases[others[0]];
                active.addTo(map);
                st.apply(active.__crs);
                noteFallback('「' + label + '」瓦片加载失败，已自动切换到「' + others[0] + '」');
              }
            }
          });
        });

        // 天地图的注记是独立图层，必须叠在底图之上
        (cfg.overlays || []).forEach(function (o) {
          var ol = tileLayer(o);
          ol.__crs = o.crs;
          ol.addTo(map);
        });
        // 世界迷雾图层：瓦片由服务端按当前底图坐标系纠偏后渲染，
        // 切换底图坐标系时必须换 URL 重新拉取。
        // 专题可以在管理页关掉迷雾（opts.showFog=false），此时图层、浓度滑杆、图层开关都不出现。
        var wantFog = opts.showFog !== false;
        var fogLayer = null;
        if (wantFog && cfg.fog && cfg.fog.blocks > 0) {
          fogLayer = L.tileLayer('/tiles/fog/{z}/{x}/{y}?crs=' + def.crs + '&v=' + cfg.fog.ver, {
            minZoom: 3, maxZoom: 18, opacity: 0.9
          });
        }
        var overlayDefs = {};
        if (fogLayer) {
          overlayDefs['世界迷雾'] = fogLayer;
          var savedFog = parseFloat(localStorage.getItem('rond.fogOpacity'));
          if (!isNaN(savedFog) && savedFog > 0 && savedFog <= 1) fogLayer.setOpacity(savedFog);
          var fogCtl = addFogOpacityControl(map, fogLayer, corner);
          // 默认打开。浓度滑杆一直摆在图上，迷雾要是默认关着，用户会以为这功能已经开了；
          // 只有用户自己关过（rond.fogOff=1）才不显示，关掉时把滑杆一并收起来。
          var syncFog = function () {
            var on = map.hasLayer(fogLayer);
            var box = fogCtl.getContainer();
            if (box) box.style.display = on ? '' : 'none';
            localStorage.setItem('rond.fogOff', on ? '0' : '1');
          };
          if (localStorage.getItem('rond.fogOff') === '1') syncFog();
          else fogLayer.addTo(map);
          map.on('overlayadd overlayremove', function (e) {
            if (e.name === '世界迷雾') syncFog();
          });
        }
        if (Object.keys(bases).length > 1 || fogLayer) {
          L.control.layers(bases, overlayDefs, { position: corner }).addTo(map);
          // 记住访客挑的底图：下次进来直接铺他喜欢的那张（含「纯路网」这种少标注的）
          map.on('baselayerchange', function (e) {
            if (e.layer) rememberBase(e.layer.__key);
          });
        }

        var group = markers(points, def.crs, true, true);
        group.addTo(map);

        // 专题页要的是「区域视野」而不是「点集视野」：只框住点的话，
        // 只去过一两个地方时视野会缩得过小，区域内的迷雾也看不全。
        // 传 fitCircle（中心 + 半径）或 fitBBox 就能覆盖默认的点集贴合。
        var applyFit = function () {
          if (opts.fitCircle && opts.fitCircle.radiusKm > 0) {
            var c = project(opts.fitCircle.lat, opts.fitCircle.lon, def.crs);
            map.fitBounds(L.latLng(c[0], c[1]).toBounds(opts.fitCircle.radiusKm * 2000));
            return;
          }
          if (opts.fitBBox) {
            var b = opts.fitBBox;
            var p1 = project(b.minLat, b.minLon, def.crs), p2 = project(b.maxLat, b.maxLon, def.crs);
            map.fitBounds(L.latLngBounds([[p1[0], p1[1]], [p2[0], p2[1]]]).pad(0.12));
            return;
          }
          fit(map, points, def.crs);
        };
        applyFit();

        var st = state4(map, points, group, def.crs, true);
        st.clustered = true;
        st.pad = 0.15;
        st.apply = function (next) {
          if (next === this.crs) return;
          this.crs = next;
          this.map.removeLayer(this.group);
          this.group = markers(this.points, this.crs, true, this.vote);
          this.group.addTo(this.map);
          applyFit();
        };
        window.rondMap._state[opts.id] = st;

        // 行程轨迹与到访热力：都基于 GCJ-02 数据，随底图坐标系重投影
        var trackLayer = null, trackSegs = null, heatLayer = null;
        var cityLayer = null, gpxLayer = null, modeLegend = null;
        function buildTrack(segs, crs) {
          var g = L.layerGroup();
          segs.forEach(function (s) {
            var a = project(s.a[0], s.a[1], crs), b = project(s.b[0], s.b[1], crs);
            L.polyline([a, b], { color: s.k || '#30b0c7', weight: 2, opacity: 0.55 })
              .bindTooltip((s.m || '行程') + (s.km ? ' · ' + s.km.toFixed(1) + ' km' : ''), { sticky: true })
              .addTo(g);
          });
          return g;
        }
        function buildHeat(crs) {
          // 必须用 st.points：本页首屏是用 points: [] 构造的（地点随后经 /api/points 拉回），
          // setPoints 只更新了 st.points，闭包里那个 points 一直是空数组 —— 用它热力图恒为空。
          var pts = (st.points || []).map(function (p) {
            var c = project(p.lat, p.lon, crs);
            return [c[0], c[1], Math.min(p.v || 1, 25)];
          }).filter(function (c) { return c[0] || c[1]; });
          return L.heatLayer(pts, { radius: 22, blur: 18, maxZoom: 17, minOpacity: 0.35 });
        }

        window.rondMap.setTrack = function (segs, modes) {
          if (trackLayer) { map.removeLayer(trackLayer); trackLayer = null; }
          if (segs && segs.length) {
            trackSegs = segs;
            trackLayer = buildTrack(segs, st.crs).addTo(map);
          } else {
            trackSegs = null;
          }
          // 交通方式图例：告诉用户哪种颜色是哪类出行
          if (modeLegend) { map.removeControl(modeLegend); modeLegend = null; }
          if (modes && modes.length && segs && segs.length) {
            modeLegend = modeLegendControl(modes);
            modeLegend.addTo(map);
          }
        };

        // 城市级聚合：圆圈大小随到访次数增长，气泡里带「首次到达」里程碑
        window.rondMap.setCityAgg = function (on) {
          if (cityLayer) { map.removeLayer(cityLayer); cityLayer = null; }
          if (!on) return;
          fetch('/api/cities').then(function (r) { return r.json(); }).then(function (d) {
            var cities = d.cities || [];
            if (!cities.length) return;
            var max = Math.max.apply(null, cities.map(function (c) { return c.visits || 1; }));
            cityLayer = L.layerGroup();
            cities.forEach(function (c) {
              var pos = project(c.lat, c.lon, st.crs);
              var r = 8 + 22 * Math.sqrt((c.visits || 1) / max);
              L.circleMarker(pos, {
                radius: r, color: '#2f7d6e', weight: 1.5,
                fillColor: '#2f7d6e', fillOpacity: 0.18
              }).bindPopup('<b>' + c.city + '</b>' +
                (c.province ? ' · ' + c.province : '') +
                '<br>到访 ' + c.visits + ' 次 · ' + c.places + ' 个地点' +
                (c.first ? '<br>首次到达 ' + c.first : '')).addTo(cityLayer);
            });
            cityLayer.addTo(map);
          });
        };

        // GPX 轨迹：库里是 WGS-84，服务端已按当前底图坐标系转换好
        window.rondMap.setGpx = function (on) {
          if (gpxLayer) { map.removeLayer(gpxLayer); gpxLayer = null; }
          if (!on) return;
          ensureGpx(st.crs, function (tracks) {
            gpxLayer = L.layerGroup();
            tracks.forEach(function (t) {
              if (!t.points || t.points.length < 2) return;
              L.polyline(t.points, { color: '#e8590c', weight: 3, opacity: 0.8 })
                .bindTooltip(t.name + (t.km ? ' · ' + t.km.toFixed(1) + ' km' : ''), { sticky: true })
                .addTo(gpxLayer);
            });
            gpxLayer.addTo(map);
          });
        };
        window.rondMap.setHeat = function (on) {
          if (heatLayer) { map.removeLayer(heatLayer); heatLayer = null; }
          if (!on) return;
          // heat 库只在第一次开热力图时才下载，平时不进首屏
          ensureHeat(function () {
            if (L.heatLayer) heatLayer = buildHeat(st.crs).addTo(map);
          });
        };

        map.on('baselayerchange', function (e) {
          if (e.layer && e.layer.__crs) {
            active = e.layer;
            errCount = 0;
            st.apply(e.layer.__crs);
            if (fogLayer && map.hasLayer(fogLayer)) {
              fogLayer.setUrl('/tiles/fog/{z}/{x}/{y}?crs=' + e.layer.__crs + '&v=' + cfg.fog.ver);
            }
            if (trackLayer && trackSegs) {
              map.removeLayer(trackLayer);
              trackLayer = buildTrack(trackSegs, e.layer.__crs).addTo(map);
            }
            if (heatLayer) {
              map.removeLayer(heatLayer);
              heatLayer = buildHeat(e.layer.__crs).addTo(map);
            }
            if (cityLayer) { map.removeLayer(cityLayer); cityLayer = null; window.rondMap.setCityAgg(true); }
            if (gpxLayer) { map.removeLayer(gpxLayer); gpxLayer = null; window.rondMap.setGpx(true); }
          }
        });

        // byId 记住「点 → 图层」，setPoints 才能只补差集；
        // max 是当前这批里最多的到访次数，用于点的半径归一化
        st.byId = {};
        st.max = 1;

        window.rondMap.setPoints = function (pts, opts) {
          opts = opts || {};
          var prev = st.byId || {};
          var max = 1;
          pts.forEach(function (p) { if (p.v > max) max = p.v; });

          // 数据是重新拉的（对象不是同一个）就整体重建：挂着的 popup 里存的是旧对象，
          // 复用会导致打开气泡看到的还是上一次筛选的数字。
          // 时间轴只是在这批点上做子集筛选，对象是同一批，因此能安全走增量。
          var refetch = pts.some(function (p) { return prev[p.id] && prev[p.id].__p !== p; });
          if (refetch || Object.keys(prev).length === 0) {
            points = st.points = pts;
            map.removeLayer(st.group);
            st.byId = {};
            st.group = markers(pts, st.crs, true, true, st.byId);
            st.group.addTo(map);
            st.max = max;
            fit(map, pts, st.crs);
            return;
          }

          var next = {};
          var added = [];
          pts.forEach(function (p) {
            var m = prev[p.id];
            if (m) {
              next[p.id] = m;
              delete prev[p.id];
              return;
            }
            m = makeMarker(p, st.crs, max, true);
            next[p.id] = m;
            added.push(m);
            st.group.addLayer(m);
          });
          // 剩下的就是这一批里没有的点，逐个移除（不重建，其余点纹丝不动）
          Object.keys(prev).forEach(function (id) { st.group.removeLayer(prev[id]); });
          // 到访次数上限变了就顺手调一下半径，免得新旧点大小不一致
          if (max !== st.max) {
            pts.forEach(function (p) {
              var m = next[p.id];
              if (m && m.setRadius) m.setRadius(radiusOf(p, max));
            });
            st.max = max;
          }
          st.byId = next;
          points = st.points = pts;
          animateIn(added);
          st.clusterIcons = clusterState(map, st.clusterIcons);
          // 时间轴拖动的每一帧都重新 fit 会让地图不停缩放，很晕；只有换筛选时才 fit
          if (!opts.keepView) fit(map, pts, st.crs);
        };
        window.addEventListener('resize', function () { map.invalidateSize(); });
      });

      return map;
    },

    _state: {}
  };
})();
