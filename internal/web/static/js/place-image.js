/* 地点图片：在浏览器里转成 WebP 再上传。
 *
 * 为什么放在前端转：1 核的服务器解一张 4000px 的原图要好几秒，几张就够卡住整站；
 * 而用户自己的手机/电脑转一张是几十毫秒的事。服务端只负责把收到的 WebP 字节原样写出去。
 *
 * 尺寸与质量策略（在真机照片上量过才定的）：
 *   长边 1440px，质量 0.72 → 超 220KB 再试 0.58 → 还超就把整张缩到 1080px 重编。
 * 关键在于「体积大头是像素数，不是质量」：实测一张 13MB 的城市照片，
 * 1600px 从 q0.74 一路降到 q0.5 只从 483KB 掉到 391KB（掉 19%，画面却明显糊了），
 * 而同样 q0.72 把长边从 1600 缩到 1024 直接掉到 217KB（掉 55%）。
 * 所以阶梯走到底还超标时，宁可缩尺寸也不再压质量。
 * 这几张图只用于详情页画廊与灯箱（最大显示到一千出头的宽度），1440px 留了余量。
 */
(function () {
  var MAX_EDGE = 1440;
  var QUALITY_LADDER = [0.72, 0.58];
  var TARGET_BYTES = 220 * 1024;
  // 兜底档：纹理特别密的照片在 1440px 下怎么降质都超标时才用
  var FALLBACK_EDGE = 1080;
  var FALLBACK_QUALITY = 0.7;

  function loadImage(file) {
    return new Promise(function (resolve, reject) {
      var url = URL.createObjectURL(file);
      var img = new Image();
      img.onload = function () { URL.revokeObjectURL(url); resolve(img); };
      img.onerror = function () { URL.revokeObjectURL(url); reject(new Error('这张图片读不出来')); };
      img.src = url;
    });
  }

  function encode(cv, quality) {
    return new Promise(function (resolve, reject) {
      cv.toBlob(function (blob) {
        // 不支持 WebP 编码的浏览器会静默给回 PNG，这里必须显式挡住——
        // 否则上传的是 PNG，服务端会以「只接受 WebP」拒掉，用户却看不出原因
        if (!blob || blob.type !== 'image/webp') {
          reject(new Error('这个浏览器不支持 WebP 转码，请换新版 Chrome / Edge / Safari'));
          return;
        }
        resolve(blob);
      }, 'image/webp', quality);
    });
  }

  // render 把原图按长边上限放到一张画布上，返回画布与落地的宽高
  function render(img, limit) {
    var w = img.naturalWidth || img.width || 1;
    var h = img.naturalHeight || img.height || 1;
    var scale = Math.min(1, limit / Math.max(w, h));
    var tw = Math.max(1, Math.round(w * scale));
    var th = Math.max(1, Math.round(h * scale));
    var cv = document.createElement('canvas');
    cv.width = tw;
    cv.height = th;
    var g = cv.getContext('2d');
    if (g.imageSmoothingQuality !== undefined) g.imageSmoothingQuality = 'high';
    // 白底：源图带透明通道时（截图、PNG）不会在 WebP 里变成黑块
    g.fillStyle = '#fff';
    g.fillRect(0, 0, tw, th);
    g.drawImage(img, 0, 0, tw, th);
    return { cv: cv, width: tw, height: th };
  }

  // toWebP(file) -> Promise<{blob, width, height, quality, bytes}>
  function toWebP(file, opts) {
    opts = opts || {};
    var maxEdge = opts.maxEdge || MAX_EDGE;
    var target = opts.targetBytes || TARGET_BYTES;
    var ladder = opts.ladder || QUALITY_LADDER;
    var fallbackEdge = opts.fallbackEdge === undefined ? FALLBACK_EDGE : opts.fallbackEdge;
    var fallbackQuality = opts.fallbackQuality || FALLBACK_QUALITY;
    return loadImage(file).then(function (img) {
      var box = render(img, maxEdge);
      function attempt(i) {
        var q = ladder[i];
        return encode(box.cv, q).then(function (blob) {
          var out = { blob: blob, width: box.width, height: box.height, quality: q, bytes: blob.size };
          if (out.bytes <= target || i === ladder.length - 1) return out;
          return attempt(i + 1);
        });
      }
      return attempt(0).then(function (out) {
        // 阶梯走到底还超标 ⇒ 这张图纹理特别密，再降质量只会又糊又大，改成缩尺寸
        var longest = Math.max(box.width, box.height);
        if (out.bytes <= target || !fallbackEdge || longest <= fallbackEdge) return out;
        var small = render(img, fallbackEdge);
        return encode(small.cv, fallbackQuality).then(function (blob) {
          // 兜底档同样是有损重编，纹理极端的图上未必比阶梯档小，所以比一次体积、取小的
          if (blob.size >= out.bytes) return out;
          return { blob: blob, width: small.width, height: small.height, quality: fallbackQuality, bytes: blob.size };
        });
      });
    });
  }

  function post(url, form) {
    return fetch(url, { method: 'POST', body: form, credentials: 'same-origin' })
      .then(function (r) { return r.json(); });
  }

  // upload(srcPK, blob, w, h) -> Promise<{ok, id, url} | {error}>
  function upload(srcPK, blob, w, h) {
    var fd = new FormData();
    fd.append('src_pk', srcPK);
    fd.append('w', w);
    fd.append('h', h);
    fd.append('file', blob, 'image.webp');
    return post('/admin/place/images', fd);
  }

  function remove(id) {
    var fd = new FormData();
    fd.append('id', id);
    return post('/admin/place/images/delete', fd);
  }

  window.rondImage = { toWebP: toWebP, upload: upload, remove: remove };
})();
