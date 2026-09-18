// 浏览器端读取照片的 EXIF（拍摄时间 + GPS），只读文件头附近的一小段，
// 不把整张照片传到服务器——几十张手机照片动辄几百 MB，上传既慢又占磁盘。
//
// 支持的容器：JPEG（APP1 段）与 HEIC/HEIF（ISO BMFF 的 meta box）。
// 算法与 internal/exif 的 Go 实现一致，但这里必须分片读取：
//   JPEG 的 APP1 在最前面，通常 < 64KB；
//   HEIC 的 Exif item 位置由 iloc 指定（绝对偏移），可能落在文件靠后，
//   所以先读 64KB 解析出偏移，必要时再按需读那一小段。
(function () {
  'use strict';

  var HEAD_BYTES = 64 * 1024;

  function readRange(file, start, end) {
    var blob = file.slice(start, Math.min(end, file.size));
    return blob.arrayBuffer().then(function (buf) {
      return new Uint8Array(buf);
    });
  }

  function str(u8, off, len) {
    var s = '';
    for (var i = 0; i < len && off + i < u8.length; i++) s += String.fromCharCode(u8[off + i]);
    return s;
  }

  // ---------- TIFF（JPEG 与 HEIC 共用，HEIC 里是 Exif item 的负载） ----------

  function readIFD(dv, u8, base, off, le) {
    var out = {};
    if (off < 0 || base + off + 2 > u8.length) return out;
    var n = dv.getUint16(base + off, le);
    var p0 = base + off + 2;
    if (p0 + n * 12 > u8.length) n = Math.max(0, Math.floor((u8.length - p0) / 12));
    for (var i = 0; i < n; i++) {
      var p = p0 + i * 12;
      out[dv.getUint16(p, le)] = {
        type: dv.getUint16(p + 2, le),
        count: dv.getUint32(p + 4, le),
        valOff: p + 8
      };
    }
    return out;
  }

  function typeSize(t) {
    switch (t) {
      case 1: case 2: case 6: case 7: return 1;
      case 3: case 8: return 2;
      case 4: case 9: case 11: return 4;
      case 5: case 10: case 12: return 8;
    }
    return 1;
  }

  // ascii 读 ASCII 值：总长 ≤ 4 字节时内联在条目里，否则 valOff 是偏移
  function ascii(dv, u8, base, e, le) {
    var total = e.count;
    if (total <= 0) return '';
    var start;
    if (total <= 4) {
      start = e.valOff;
    } else {
      start = base + dv.getUint32(e.valOff, le);
    }
    var out = '';
    for (var i = 0; i < total && start + i < u8.length; i++) {
      var c = u8[start + i];
      if (c === 0) break;
      out += String.fromCharCode(c);
    }
    return out;
  }

  function rationals(dv, u8, base, e, le, n) {
    var off = base + dv.getUint32(e.valOff, le);
    if (off < 0 || off + n * 8 > u8.length) return null;
    var out = [];
    for (var i = 0; i < n; i++) {
      var num = dv.getUint32(off + i * 8, le);
      var den = dv.getUint32(off + i * 8 + 4, le);
      if (den === 0) return null;
      out.push(num / den);
    }
    return out;
  }

  // gpsCoord 把「度分秒 + 半球」拼成十进制度
  function gpsCoord(dv, u8, base, le, gps, refTag, valTag) {
    var refE = gps[refTag], valE = gps[valTag];
    if (!refE || !valE) return null;
    var parts = rationals(dv, u8, base, valE, le, 3);
    if (!parts) return null;
    var v = parts[0] + parts[1] / 60 + parts[2] / 3600;
    var ref = ascii(dv, u8, base, refE, le).toUpperCase();
    if (ref === 'S' || ref === 'W') v = -v;
    return v;
  }

  function parseTIFF(u8, base) {
    var dv = new DataView(u8.buffer, u8.byteOffset, u8.byteLength);
    if (base + 8 > u8.length) throw new Error('EXIF 数据不完整');
    var mark = str(u8, base, 2);
    if (mark !== 'II' && mark !== 'MM') throw new Error('EXIF 标识不对');
    var le = mark === 'II';
    if (dv.getUint16(base + 2, le) !== 0x002A) throw new Error('EXIF 标识不对');

    var res = { hasGPS: false, hasTime: false, lat: 0, lon: 0, shot: null };
    var ifd0 = readIFD(dv, u8, base, dv.getUint32(base + 4, le), le);

    if (ifd0[0x8769]) {
      var sub = readIFD(dv, u8, base, dv.getUint32(ifd0[0x8769].valOff, le), le);
      if (sub[0x9003]) {
        var m = /^(\d{4}):(\d{2}):(\d{2})[ T](\d{2}):(\d{2}):(\d{2})/.exec(ascii(dv, u8, base, sub[0x9003], le).trim());
        if (m) {
          res.shot = new Date(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6]);
          res.hasTime = true;
        }
      }
    }
    if (ifd0[0x8825]) {
      var gps = readIFD(dv, u8, base, dv.getUint32(ifd0[0x8825].valOff, le), le);
      var la = gpsCoord(dv, u8, base, le, gps, 1, 2);
      var lo = gpsCoord(dv, u8, base, le, gps, 3, 4);
      if (la !== null && lo !== null && (la !== 0 || lo !== 0)) {
        res.lat = la; res.lon = lo; res.hasGPS = true;
      }
    }
    return res;
  }

  // ---------- JPEG ----------

  function parseJPEG(u8) {
    var dv = new DataView(u8.buffer, u8.byteOffset, u8.byteLength);
    var i = 2;
    while (i + 4 <= u8.length) {
      if (u8[i] !== 0xFF) { i++; continue; }
      var marker = u8[i + 1];
      if (marker === 0xD8 || marker === 0x01 || (marker >= 0xD0 && marker <= 0xD7)) { i += 2; continue; }
      if (marker === 0xDA || marker === 0xD9) break; // 图像数据开始
      var size = (u8[i + 2] << 8) | u8[i + 3];
      if (size < 2 || i + 2 + size > u8.length) break;
      if (marker === 0xE1 && size > 8 && str(u8, i + 4, 6) === 'Exif\x00\x00') {
        return parseTIFF(u8, i + 10);
      }
      i += 2 + size;
    }
    throw new Error('这张 JPEG 里没有 EXIF 信息');
  }

  // ---------- HEIC / HEIF ----------

  function boxHeader(dv, u8, off) {
    if (off + 8 > u8.length) return null;
    var size = dv.getUint32(off);
    var hdr = 8;
    if (size === 1) {
      if (off + 16 > u8.length) return null;
      size = Number(dv.getBigUint64(off + 8));
      hdr = 16;
    } else if (size === 0) {
      size = u8.length - off;
    }
    if (size < hdr) return null;
    // next 允许超出已读范围：大文件的 meta 往往只读到头部，后面的数据按需再取
    return { typ: str(u8, off + 4, 4), payload: off + hdr, next: off + size };
  }

  function findExifItemID(dv, u8, start, end) {
    if (start + 4 > u8.length) return 0;
    var ver = u8[start];
    var p = start + 4;
    var count = ver === 0 ? dv.getUint16(p) : dv.getUint32(p);
    p += ver === 0 ? 2 : 4;
    for (var i = 0; i < count; i++) {
      var b = boxHeader(dv, u8, p);
      if (!b) break;
      if (b.typ === 'infe' && b.payload < u8.length && u8[b.payload] >= 2 && b.payload + 12 <= u8.length) {
        if (str(u8, b.payload + 8, 4) === 'Exif') return dv.getUint16(b.payload + 4);
      }
      p = b.next;
    }
    return 0;
  }

  function findItemExtent(dv, u8, start, wantID) {
    if (start + 8 > u8.length) return null;
    var ver = u8[start];
    var p = start + 4;
    var offsetSize = u8[p] >> 4, lengthSize = u8[p] & 0x0F;
    p++;
    var baseSize = u8[p] >> 4, indexSize = ver >= 1 ? (u8[p] & 0x0F) : 0;
    p++;
    if (!offsetSize || !lengthSize) return null;
    var count = dv.getUint16(p);
    p += 2;
    function readN(n) {
      if (n < 0 || p + n > u8.length) return null;
      var v = 0;
      for (var i = 0; i < n; i++) v = v * 256 + u8[p + i];
      p += n;
      return v;
    }
    for (var i = 0; i < count; i++) {
      var id = ver === 0 ? readN(2) : readN(4);
      if (id === null) return null;
      if (ver >= 1 && readN(2) === null) return null;
      if (readN(2) === null) return null;            // data_reference_index
      var base = readN(baseSize);
      if (base === null) return null;
      var extents = readN(2);
      if (extents === null) return null;
      for (var e = 0; e < extents; e++) {
        if (ver >= 1 && indexSize > 0 && readN(indexSize) === null) return null;
        var off = readN(offsetSize), len = readN(lengthSize);
        if (off === null || len === null) return null;
        if (id === wantID) return { offset: base + off, length: len };
      }
    }
    return null;
  }

  function parseHEIF(file, u8) {
    var dv = new DataView(u8.buffer, u8.byteOffset, u8.byteLength);
    var meta = null;
    for (var off = 0; off + 8 <= u8.length;) {
      var b = boxHeader(dv, u8, off);
      if (!b) break;
      if (b.typ === 'meta') { meta = b; break; }
      off = b.next;
    }
    if (!meta) throw new Error('HEIC 文件里没有找到 meta');
    var start = meta.payload + 4; // meta 是 FullBox

    var iinf = null, iloc = null;
    for (var o = start; o + 8 <= u8.length && o < meta.next;) {
      var sb = boxHeader(dv, u8, o);
      if (!sb) break;
      if (sb.typ === 'iinf') iinf = sb;
      if (sb.typ === 'iloc') iloc = sb;
      o = sb.next;
    }
    if (!iinf || !iloc) throw new Error('HEIC 里缺少 iinf/iloc');

    var itemID = findExifItemID(dv, u8, iinf.payload, iinf.next);
    if (!itemID) throw new Error('这张 HEIC 里没有 EXIF');
    var ext = findItemExtent(dv, u8, iloc.payload, itemID);
    if (!ext || ext.length <= 8) throw new Error('HEIC 的 EXIF 位置无效');

    function finish(bytes) {
      var tiff = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getUint32(0);
      if (tiff + 8 > bytes.length) tiff = 4;
      return parseTIFF(bytes, tiff);
    }
    // Exif item 已经读进来了就直接解析，否则按偏移再取那一小段
    if (ext.offset >= 0 && ext.offset + ext.length <= u8.length) {
      return finish(u8.subarray(ext.offset, ext.offset + ext.length));
    }
    return readRange(file, ext.offset, ext.offset + ext.length).then(finish);
  }

  // ---------- 对外入口 ----------

  function read(file) {
    return readRange(file, 0, HEAD_BYTES).then(function (head) {
      if (head.length >= 2 && head[0] === 0xFF && head[1] === 0xD8) {
        return Promise.resolve(parseJPEG(head));
      }
      if (head.length >= 12 && str(head, 4, 4) === 'ftyp') {
        return parseHEIF(file, head);
      }
      return Promise.reject(new Error('不认识的图片格式（支持 JPEG 与 HEIC/HEIF）'));
    });
  }

  window.rondExif = { read: read };
})();
