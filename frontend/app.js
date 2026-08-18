/* aiofiles - Alpine components, API client, SSE wiring.
 * Live updates come from EventSource only; there is no polling anywhere in
 * this file. Reconnects back off exponentially and reconcile once on open. */

var THEME_KEY = "aiofiles.theme";
var SOUND_KEY = "aiofiles.finish-sound";
var FINISH_SOUND_URL = "/assets/notification.wav";
var RELEASE_CACHE_KEY = "aiofiles.github-release";
var RELEASE_CACHE_TTL = 24 * 60 * 60 * 1000;
var RELEASES_URL = "https://api.github.com/repos/panonim/aiofiles/releases/latest";
var RELEASE_PAGE = "https://github.com/panonim/aiofiles/releases/latest";

/* Matches the field-flash animation in app.css. */
var flashMs = 1600;

/* Statuses are part of the API contract's enum; everything the server does
   publish (containers, codecs, retention, ...) comes from /api/presets.
   "all" rather than "": a bound empty value leaves <option> without a value
   attribute, so the browser falls back to the label text. */
var STATUS_FILTERS = [
  { value: "all", label: "All statuses" },
  { value: "queued", label: "Queued" },
  { value: "running", label: "Running" },
  { value: "done", label: "Done" },
  { value: "failed", label: "Failed" },
  { value: "canceled", label: "Canceled" },
  { value: "expired", label: "Expired" },
];

var TYPE_ICON = {
  download: "cloud-download",
  convert: "repeat",
  compress: "minimize-2",
  image: "image",
};

/* The two axes the URL carries: "#jobs", "#new/compress". These are tabs, not
   wire job types - Convert and Compress both send an "image" job for images. */
var JOBS_PER_PAGE = 50;

var TABS = ["new", "jobs", "settings"];
var JOB_TYPES = ["download", "convert", "compress"];

/* Browsers leave File.type empty for many media containers, so the extension
   is a fallback rather than the first guess. */
var IMAGE_EXTS = [
  "jpg", "jpeg", "png", "webp", "avif", "gif", "tif", "tiff", "bmp",
  "heic", "heif", "svg",
];

/* Mirrors imageFormatByExt in internal/presets: what "same as the input"
   resolves to, and so which entry the output list must not offer twice. */
var IMAGE_FORMAT_BY_EXT = {
  jpg: "jpg", jpeg: "jpg", png: "png", webp: "webp", avif: "avif",
  gif: "gif", tif: "tiff", tiff: "tiff", svg: "svg",
};

var AUDIO_EXTS = ["mp3", "m4a", "aac", "flac", "wav", "ogg", "oga", "opus", "wma"];

var SVG_SCALES = [0.25, 0.5, 1, 1.5, 2, 3, 4];
var SVG_SCALE_DEFAULT = 2; // index of 1x
var SVG_FALLBACK_SIZE = 1024;

/* Reads the drawing size out of an SVG: the width/height attributes when they
   are plain numbers or px, the viewBox otherwise. */
function parseSVGSize(text) {
  var tag = /<svg\b[^>]*>/i.exec(String(text || ""));
  if (!tag) return null;
  var open = tag[0];
  var num = function (attr) {
    var m = new RegExp(attr + '\\s*=\\s*"([\\d.]+)(px)?"', "i").exec(open);
    return m ? Math.round(parseFloat(m[1])) : 0;
  };
  var w = num("width");
  var h = num("height");
  if (w > 0 && h > 0) return { w: w, h: h };
  var box = /viewBox\s*=\s*"([^"]+)"/i.exec(open);
  if (box) {
    var parts = box[1].trim().split(/[\s,]+/);
    if (parts.length === 4) {
      var bw = Math.round(parseFloat(parts[2]));
      var bh = Math.round(parseFloat(parts[3]));
      if (bw > 0 && bh > 0) return { w: bw, h: bh };
    }
  }
  return null;
}

function extOf(name) {
  var dot = String(name || "").toLowerCase().lastIndexOf(".");
  return dot === -1 ? "" : String(name).toLowerCase().slice(dot + 1);
}

/* Anything not recognisably an image is treated as video: the server validates
   for real, and guessing "image" wrongly hides the controls that matter. */
function fileKind(file) {
  var type = String((file && file.type) || "").toLowerCase();
  if (type.indexOf("image/") === 0) return "image";
  if (type.indexOf("video/") === 0 || type.indexOf("audio/") === 0) {
    return "video";
  }
  var name = String((file && file.name) || "").toLowerCase();
  var dot = name.lastIndexOf(".");
  var ext = dot === -1 ? "" : name.slice(dot + 1);
  return IMAGE_EXTS.indexOf(ext) === -1 ? "video" : "image";
}

/* SubtleCrypto has no streaming digest, so hashing a whole file would mean
   holding gigabytes in memory. Small files are hashed entire; larger ones are
   identified by their byte length plus their first and last blocks. */
var HASH_FULL_MAX = 64 * 1024 * 1024;
var HASH_EDGE = 4 * 1024 * 1024;

function hex(buffer) {
  var bytes = new Uint8Array(buffer);
  var out = "";
  for (var i = 0; i < bytes.length; i++) {
    out += (bytes[i] < 16 ? "0" : "") + bytes[i].toString(16);
  }
  return out;
}

function metaPrint(file) {
  return "meta:" + file.size + ":" + file.lastModified + ":" + file.name;
}

/* crypto.subtle only exists in a secure context, and this tool is routinely
   served over plain HTTP on a LAN address; there name, size and mtime stand
   in - weaker, but still enough to catch the same file added twice. */
async function fileFingerprint(file) {
  if (!window.crypto || !window.crypto.subtle) {
    return metaPrint(file);
  }
  try {
    var blob =
      file.size <= HASH_FULL_MAX
        ? file
        : new Blob([file.slice(0, HASH_EDGE), file.slice(file.size - HASH_EDGE)]);
    var digest = await window.crypto.subtle.digest(
      "SHA-256",
      await blob.arrayBuffer(),
    );
    return file.size + ":" + hex(digest);
  } catch (e) {
    return metaPrint(file);
  }
}

var TYPE_LABEL = {
  download: "Download",
  convert: "Convert",
  compress: "Compress",
  image: "Image",
};

/* Queued and running are the states still in play; the rest are terminal. */
function isActive(job) {
  return job.status === "running" || job.status === "queued";
}

/* Lifecycle order, so a late snapshot (the create response can land after the
   SSE stream has already reported the job running) never rewinds it. */
var STATUS_RANK = { queued: 0, running: 1, done: 2, failed: 2, canceled: 2, expired: 2 };

function ApiError(message, status, field) {
  this.name = "ApiError";
  this.message = message;
  this.status = status;
  this.field = field || "";
}
ApiError.prototype = Object.create(Error.prototype);
ApiError.prototype.constructor = ApiError;

function OfflineError(message) {
  this.name = "OfflineError";
  this.message = message || "Cannot reach the server.";
}
OfflineError.prototype = Object.create(Error.prototype);
OfflineError.prototype.constructor = OfflineError;

function goToLogin() {
  if (window.location.pathname !== "/login.html") {
    window.location.assign("/login.html");
  }
}

/* Standalone iOS ignores the download attribute and navigates the one window
   it has at the file, parking the user in a viewer with no way back. Those
   saves fetch the bytes and hand them to the share sheet instead. */
function isStandalone() {
  if (window.navigator.standalone === true) return true; // iOS home screen
  if (!window.matchMedia) return false;
  return ["standalone", "fullscreen", "minimal-ui"].some(function (mode) {
    return window.matchMedia("(display-mode: " + mode + ")").matches;
  });
}

/* iPadOS calls itself a Mac; the touch points are what give it away. */
function isIOS() {
  var ua = window.navigator.userAgent || "";
  if (/iPhone|iPad|iPod/.test(ua)) return true;
  return /Macintosh/.test(ua) && (window.navigator.maxTouchPoints || 0) > 1;
}

/* Mirrors the .job phone breakpoint in app.css. */
function isMobileViewport() {
  return !!(window.matchMedia && window.matchMedia("(max-width: 680px)").matches);
}

/* A fetched file is held whole in memory; past this the OS is apt to kill the
   web view mid-save, so bigger outputs open in a browser window instead. */
var SAVE_BLOB_MAX = 512 * 1024 * 1024;

/* XMLHttpRequest for the reason uploadFile uses it: progress. The xhr comes
   back alongside the promise so a save in flight can be aborted. */
function fetchBlob(url, onProgress) {
  var xhr = new XMLHttpRequest();
  var done = new Promise(function (resolve, reject) {
    xhr.open("GET", url, true);
    xhr.withCredentials = true;
    xhr.responseType = "blob";

    xhr.onprogress = function (e) {
      if (e.lengthComputable && onProgress) {
        onProgress(Math.round((e.loaded / e.total) * 100));
      }
    };

    xhr.onload = function () {
      if (xhr.status === 401) {
        goToLogin();
        reject(new ApiError("Your session ended. Sign in again.", 401));
        return;
      }
      if (xhr.status >= 200 && xhr.status < 300 && xhr.response) {
        resolve(xhr.response);
        return;
      }
      var msg = "The file could not be fetched (" + xhr.status + ").";
      reject(new ApiError(msg, xhr.status));
    };

    xhr.onerror = function () {
      reject(new OfflineError("The download failed - the server is unreachable."));
    };
    xhr.onabort = function () {
      reject(new ApiError("Save canceled.", 0));
    };

    if (onProgress) onProgress(0);
    xhr.send();
  });
  return { xhr: xhr, done: done };
}

/* Outside the component: a Blob has no business in a reactive proxy, and only
   one save runs at a time. */
var saveRequest = null; // the xhr while bytes are still coming
var savePending = null; // the File once they have landed, until it is saved
var saveTick = 0; // retires a save whose abort lands after the next one started

function emptySave() {
  return { id: "", name: "", url: "", pct: 0, phase: "", error: "" };
}

/* A server error body is either a bare string or {field, reason}. */
function apiErrorFrom(payload, status, fallback) {
  var err = payload && payload.error;
  if (!err) return new ApiError(fallback, status);
  if (typeof err === "string") return new ApiError(err, status);
  return new ApiError(err.reason || fallback, status, err.field || "");
}

/* `opts.body` is serialised as JSON automatically. A 401 sends the browser to
   the login page unless opts.noRedirect is set. */
async function api(path, opts) {
  opts = opts || {};
  var init = {
    method: opts.method || "GET",
    credentials: "same-origin",
    headers: { Accept: "application/json" },
  };
  if (opts.body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(opts.body);
  }
  if (opts.signal) init.signal = opts.signal;

  var res;
  try {
    res = await fetch("/api" + path, init);
  } catch (e) {
    throw new OfflineError(
      "The API did not respond. Check that the server is running, then retry.",
    );
  }

  if (res.status === 401 && !opts.noRedirect) {
    goToLogin();
    throw new ApiError("Your session ended. Sign in again.", 401);
  }

  var payload = null;
  if (res.status !== 204) {
    try {
      payload = await res.json();
    } catch (e) {
      payload = null;
    }
  }

  if (!res.ok) {
    throw apiErrorFrom(payload, res.status, "Request failed (" + res.status + ").");
  }
  return payload;
}

/* XMLHttpRequest rather than fetch(): fetch cannot observe upload progress. */
function uploadFile(file, onProgress) {
  return new Promise(function (resolve, reject) {
    var form = new FormData();
    form.append("file", file);

    var xhr = new XMLHttpRequest();
    xhr.open("POST", "/api/uploads", true);
    xhr.withCredentials = true;
    xhr.responseType = "json";

    xhr.upload.onprogress = function (e) {
      if (e.lengthComputable && onProgress) {
        onProgress(Math.round((e.loaded / e.total) * 100));
      }
    };

    xhr.onload = function () {
      var body = xhr.response;
      if (typeof body === "string") {
        try {
          body = JSON.parse(body);
        } catch (e) {
          body = null;
        }
      }
      if (xhr.status === 401) {
        goToLogin();
        reject(new ApiError("Your session ended. Sign in again.", 401));
        return;
      }
      if (xhr.status >= 200 && xhr.status < 300 && body) {
        resolve(body);
        return;
      }
      reject(
        apiErrorFrom(body, xhr.status, "Upload failed (" + xhr.status + ")."),
      );
    };

    xhr.onerror = function () {
      reject(new OfflineError("Upload failed - the server is unreachable."));
    };
    xhr.onabort = function () {
      reject(new ApiError("Upload canceled.", 0));
    };

    if (onProgress) onProgress(0);
    xhr.send(form);
  });
}

function humanBytes(n) {
  if (n === null || n === undefined || n === "" || isNaN(n)) return "-";
  n = Number(n);
  if (n <= 0) return "0 B";
  var units = ["B", "KB", "MB", "GB", "TB"];
  var i = Math.floor(Math.log(n) / Math.log(1024));
  if (i > units.length - 1) i = units.length - 1;
  var v = n / Math.pow(1024, i);
  var digits = v >= 100 || i === 0 ? 0 : 1;
  return v.toFixed(digits) + " " + units[i];
}

function humanDuration(seconds) {
  if (!seconds || isNaN(seconds)) return "-";
  var s = Math.floor(Number(seconds));
  var h = Math.floor(s / 3600);
  var m = Math.floor((s % 3600) / 60);
  var sec = s % 60;
  var pad = function (x) {
    return x < 10 ? "0" + x : String(x);
  };
  return h > 0 ? h + ":" + pad(m) + ":" + pad(sec) : m + ":" + pad(sec);
}

function relativeTime(iso, now) {
  if (!iso) return "-";
  var then = new Date(iso).getTime();
  if (isNaN(then)) return "-";
  var diff = Math.round(((now || Date.now()) - then) / 1000);
  var future = diff < 0;
  var d = Math.abs(diff);
  var out;
  if (d < 45) out = "just now";
  else if (d < 90) out = "1 min";
  else if (d < 3600) out = Math.round(d / 60) + " min";
  else if (d < 7200) out = "1 hour";
  else if (d < 86400) out = Math.round(d / 3600) + " hours";
  else if (d < 172800) out = "1 day";
  else out = Math.round(d / 86400) + " days";
  if (out === "just now") return future ? "in a moment" : out;
  return future ? "in " + out : out + " ago";
}

function absoluteTime(iso) {
  if (!iso) return "";
  var d = new Date(iso);
  return isNaN(d.getTime()) ? "" : d.toLocaleString();
}

function applyTheme(choice) {
  var resolved = choice;
  if (choice !== "light" && choice !== "dark") {
    resolved = window.matchMedia("(prefers-color-scheme: dark)").matches
      ? "dark"
      : "light";
  }
  document.documentElement.setAttribute("data-theme", resolved);
  return resolved;
}

function storedTheme() {
  try {
    var v = localStorage.getItem(THEME_KEY);
    return v === "light" || v === "dark" ? v : "system";
  } catch (e) {
    return "system";
  }
}

/* On unless it was turned off, so the setting is discovered by hearing it. */
function storedSound() {
  try {
    return localStorage.getItem(SOUND_KEY) !== "off";
  } catch (e) {
    return true;
  }
}

/* One element, reused: a fresh Audio per play leaks decoders on a long batch. */
var finishAudio = null;
function playFinishSound() {
  try {
    if (!finishAudio) finishAudio = new Audio(FINISH_SOUND_URL);
    finishAudio.currentTime = 0;
    var p = finishAudio.play();
    /* A tab that never saw a gesture is refused by autoplay policy; silence
       is the correct outcome there, not an error. */
    if (p && p.catch) p.catch(function () {});
  } catch (e) {
    /* no audio available */
  }
}

function releaseNumber(value) {
  var match = String(value || "").trim().replace(/^v/i, "").match(/^(\d+)\.(\d+)\.(\d+)/);
  return match
    ? [Number(match[1]), Number(match[2]), Number(match[3])]
    : null;
}

function newerRelease(latest, current) {
  var a = releaseNumber(latest);
  var b = releaseNumber(current);
  if (!a || !b) return false;
  for (var i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return a[i] > b[i];
  }
  return false;
}

function drawIcons() {
  if (window.lucide && typeof window.lucide.createIcons === "function") {
    window.lucide.createIcons();
  }
}

function mediaApp() {
  return {
    tab: "new",
    jobType: "download",
    offline: "",
    presets: null,
    updateAvailable: false,
    updateURL: RELEASE_PAGE,
    health: { status: "", auth_required: false },
    me: { authenticated: false, username: "", auth_required: false },
    statusFilters: STATUS_FILTERS,
    theme: storedTheme(),
    finishSound: storedSound(),
    now: Date.now(),

    es: null,
    live: {},
    linked: false,
    backoff: 1000,
    reconnectTimer: null,
    everLinked: false,

    jobs: [],
    jobFilter: "all",
    jobPage: 1,
    picked: {},
    bulkBusy: false,
    jobsLoading: false,
    announce: "",
    jobsError: "",
    /* Guards a row against a second delete while the first is in flight. */
    deleting: {},
    /* Retires a list response that was overtaken by a newer load or a delete. */
    jobsSeq: 0,
    /* id -> when it was deleted, so a stale snapshot cannot bring it back. */
    recentlyDeleted: {},
    /* Ids of jobs started from this tab, newest first, watched in the docked
       queue panel. `jobs` stays the single source of truth. */
    queue: [],
    queueOpen: false,
    /* One managed save at a time. phase: "" | "fetching" | "ready" | "error",
       where "ready" means the bytes are in hand and a tap is owed. */
    save: emptySave(),
    /* The open preview overlay: null when closed, else {job, kind, url}. */
    preview: null,

    submitting: "",
    /* Keyed by tab: an image job from either file tab reports its errors
       under that tab, not under a type of its own. */
    errors: { download: {}, convert: {}, compress: {} },
    /* Per form, field name -> a counter, non-zero while the field is lit. */
    flash: { convert: {}, compress: {} },
    formError: { download: "", convert: "", compress: "" },

    probe: { url: "", busy: false, result: null, error: "" },
    dl: {
      mode: "video",
      format_id: "",
      audio_format: "",
      audio_bitrate: "192",
      container: "",
      embed_subs: false,
      embed_thumbnail: true,
      embed_metadata: true,
      retention_days: 7,
      /* Best available is nearly always right, so the table starts folded. */
      showFormats: false,
    },

    /* Keyed by tab, not by job type - an upload carries a `kind` of "video" or
       "image" that decides the options shown and the job type submitted. */
    uploads: {
      convert: [],
      compress: [],
    },
    dragging: { convert: false, compress: false },
    uploadSeq: { convert: 0, compress: 0 },

    library: { open: false, tab: "", loading: false, error: "", files: [], busy: "" },

    forms: {
      convert: {
        container: "mp4",
        preset: "balanced",
        video_codec: "h264",
        speed: "medium",
        crf: 23,
        resolution: "source",
        frame_rate: "source",
        audio_codec: "aac",
        audio_bitrate: "192",
        strip_metadata: false,
        retention_days: 7,
      },
      compress: {
        target: "balanced",
        target_size_mb: 100,
        container: "mp4",
        video_codec: "h264",
        speed: "medium",
        resolution: "source",
        audio_bitrate: "128",
        strip_metadata: false,
        retention_days: 7,
        width: 0,
        height: 0,
      },
      image: {
        format: "webp",
        quality: 100,
        quality_preset: "high",
        trace: "dark",
        svg_scale: SVG_SCALE_DEFAULT,
        width: 0,
        height: 0,
        strip_metadata: true,
        retention_days: 7,
      },
    },

    async init() {
      applyTheme(this.theme);
      var self = this;

      /* Read the URL before the first paint so a shared or reloaded link lands
         on the right tab, then normalise it without pushing a history entry. */
      var start = this.readHash();
      this.tab = start.tab;
      this.jobType = start.type;
      this.writeHash(true);

      window.addEventListener("hashchange", function () {
        self.applyHash();
      });

      /* The whole batch landing, not each file: the last active job in the
         queue settling is the only moment worth a sound. */
      this.$watch("queueRunning", function (now, was) {
        if (now !== 0 || !was || !self.queue.length) return;
        if (!self.finishSound) return;
        /* "Not focused" covers a background tab and a window behind another
           one, which are the cases where the user cannot see the queue. */
        if (!document.hidden && document.hasFocus()) return;
        playFinishSound();
      });

      window
        .matchMedia("(prefers-color-scheme: dark)")
        .addEventListener("change", function () {
          if (self.theme === "system") applyTheme("system");
        });

      /* Relative timestamps refresh on real events, never on a timer. */
      var hiddenAt = 0;
      document.addEventListener("visibilitychange", function () {
        if (document.hidden) {
          hiddenAt = Date.now();
          return;
        }
        self.now = Date.now();
        self.refreshIcons();
        self.resume(hiddenAt);
        hiddenAt = 0;
      });

      /* A page out of the back cache comes back with its DOM intact and its
         sockets dead, and so does a suspended installed app. */
      window.addEventListener("pageshow", function (e) {
        if (e.persisted) self.resume(0);
      });
      window.addEventListener("online", function () {
        self.resume(0);
      });

      window.addEventListener("beforeunload", function () {
        if (self.es) self.es.close();
      });

      await this.bootstrap();
      this.refreshIcons();
    },

    async bootstrap() {
      try {
        var results = await Promise.all([
          api("/health"),
          api("/auth/me"),
          api("/presets"),
        ]);
        this.health = results[0] || this.health;
        this.me = results[1] || this.me;
        this.presets = results[2];
        this.checkForUpdate();
        this.offline = "";

        if (this.me.auth_required && !this.me.authenticated) {
          goToLogin();
          return;
        }

        this.applyPresetDefaults();
        await this.loadJobs();
        this.connectEvents();
      } catch (e) {
        if (e instanceof OfflineError) {
          this.offline = e.message;
        } else if (e.status !== 401) {
          this.offline = e.message || "Something went wrong loading the app.";
        }
      }
    },

    /* Seed every default from what the server serves, falling back to the
       first allowed option. */
    applyPresetDefaults() {
      var p = this.presets;
      if (!p) return;
      var days = Number(p.default_retention_days);
      if (!isNaN(days)) {
        this.dl.retention_days = days;
        this.forms.convert.retention_days = days;
        this.forms.compress.retention_days = days;
        this.forms.image.retention_days = days;
      }
      var first = function (list, current) {
        if (!list || !list.length) return current;
        for (var i = 0; i < list.length; i++) {
          if (list[i].value === current) return current;
        }
        return list[0].value;
      };
      this.dl.audio_format = first(p.audio_formats, this.dl.audio_format);
      this.dl.audio_bitrate = first(p.audio_bitrates, this.dl.audio_bitrate);
      this.dl.container = first(p.video_containers, this.dl.container);

      var c = this.forms.convert;
      c.container = first(p.video_containers, c.container);
      c.preset = first(p.encode_presets, c.preset);
      c.video_codec = first(p.video_codecs, c.video_codec);
      c.speed = first(p.encode_speeds, c.speed);
      c.resolution = first(p.resolutions, c.resolution);
      c.frame_rate = first(p.frame_rates, c.frame_rate);
      c.audio_codec = first(p.audio_codecs, c.audio_codec);
      c.audio_bitrate = first(p.audio_bitrates, c.audio_bitrate);

      var z = this.forms.compress;
      z.target = first(p.compress_targets, z.target);
      z.container = first(p.video_containers, z.container);
      z.video_codec = first(p.video_codecs, z.video_codec);
      z.speed = first(p.encode_speeds, z.speed);
      z.resolution = first(p.resolutions, z.resolution);
      z.audio_bitrate = first(p.audio_bitrates, z.audio_bitrate);

      this.forms.image.format = first(p.image_formats, this.forms.image.format);
      this.forms.image.quality_preset = first(p.image_quality_presets, this.forms.image.quality_preset);
      this.applyPreset();
      this.applyCompressTarget(true); // silent: nothing to point at on load
      this.applyImagePreset(true); // silent: nothing to point at on load
      this.clampCRF();
    },

    async checkForUpdate() {
      var current = this.presets && this.presets.version;
      if (
        !current ||
        current === "dev" ||
        (this.presets && this.presets.update_checks_disabled)
      ) {
        this.updateAvailable = false;
        return;
      }

      var cached = null;
      try {
        cached = JSON.parse(localStorage.getItem(RELEASE_CACHE_KEY) || "null");
      } catch (e) {}

      if (
        cached &&
        cached.current === current &&
        Number(cached.checked_at) > Date.now() - RELEASE_CACHE_TTL
      ) {
        this.updateAvailable = newerRelease(cached.latest_tag, current);
        this.updateURL = cached.latest_url || RELEASE_PAGE;
        return;
      }

      try {
        var response = await fetch(RELEASES_URL, {
          headers: { Accept: "application/vnd.github+json" },
          cache: "no-store",
        });
        if (!response.ok) throw new Error("GitHub release lookup failed");
        var release = await response.json();
        var latestTag = String(release.tag_name || "");
        var latestURL = release.html_url || RELEASE_PAGE;
        this.updateAvailable = newerRelease(latestTag, current);
        this.updateURL = latestURL;
        try {
          localStorage.setItem(
            RELEASE_CACHE_KEY,
            JSON.stringify({
              current: current,
              latest_tag: latestTag,
              latest_url: latestURL,
              checked_at: Date.now(),
            }),
          );
        } catch (e) {}
      } catch (e) {
        /* Cache the failed check too, so GitHub is not retried on every load. */
        try {
          localStorage.setItem(
            RELEASE_CACHE_KEY,
            JSON.stringify({ current: current, checked_at: Date.now() }),
          );
        } catch (ignored) {}
      }
    },

    refreshIcons() {
      this.$nextTick(function () {
        drawIcons();
      });
    },

    /* The tab lives in the URL so the browser's own back gesture moves between
       sections; without it, back on a phone leaves the app. */
    readHash() {
      var raw = String(window.location.hash || "").replace(/^#\/?/, "");
      var parts = raw.split("/");
      return {
        tab: TABS.indexOf(parts[0]) !== -1 ? parts[0] : this.tab,
        type: JOB_TYPES.indexOf(parts[1]) !== -1 ? parts[1] : this.jobType,
      };
    },

    /* replace: true rewrites the current entry (boot, normalisation);
       false pushes a new one, which is what makes back work. */
    writeHash(replace) {
      var want = "#" + this.tab + (this.tab === "new" ? "/" + this.jobType : "");
      if (window.location.hash === want) return;
      try {
        if (replace && window.history && window.history.replaceState) {
          window.history.replaceState(null, "", want);
          return;
        }
      } catch (e) {
        /* fall through to the assignment below */
      }
      window.location.hash = want;
    },

    /* Runs on hashchange, including the ones writeHash triggers itself - those
       land here as no-ops because the state already matches. */
    applyHash() {
      var h = this.readHash();
      if (h.tab === this.tab && h.type === this.jobType) return;
      this.tab = h.tab;
      this.jobType = h.type;
      this.now = Date.now();
      this.scrollTop();
      this.refreshIcons();
    },

    go(tab) {
      var moved = this.tab !== tab;
      this.tab = tab;
      this.now = Date.now();
      this.writeHash(false);
      if (moved) this.scrollTop();
      this.refreshIcons();
    },

    pickType(type) {
      this.jobType = type;
      this.writeHash(false);
      this.scrollTop();
      this.refreshIcons();
    },

    /* Switching sections otherwise keeps the previous section's scroll offset,
       landing the user halfway down the new one. */
    scrollTop() {
      if (typeof window.scrollTo === "function") window.scrollTo(0, 0);
    },

    options(name) {
      return (this.presets && this.presets[name]) || [];
    },

    /* The format the batch already is, or "" when the batch is mixed. */
    uploadImageFormat(tab) {
      var arr = this.uploads[tab];
      if (!arr.length) return "";
      var first = IMAGE_FORMAT_BY_EXT[extOf(arr[0].name)] || "";
      for (var i = 1; i < arr.length; i++) {
        if ((IMAGE_FORMAT_BY_EXT[extOf(arr[i].name)] || "") !== first) return "";
      }
      return first;
    },

    /* "Same as the input" already covers PNG -> PNG, so the explicit entry is
       dropped rather than offered as a second way to say the same thing. An SVG
       input drops every SVG output with it: all three would only re-wrap a
       render of the vectors that were already there. */
    imageFormatOptions() {
      var all = this.options("image_formats");
      var same = this.uploadImageFormat("convert");
      if (!same) return all;
      var hidden = same === "svg" ? ["svg", "svg_trace", "source"] : [same];
      return all.filter(function (o) {
        return hidden.indexOf(o.value) === -1;
      });
    },

    get svgInput() {
      return this.uploadImageFormat("convert") === "svg";
    },

    /* The size the file itself declares, or a sane render default. */
    svgNatural() {
      var arr = this.uploads.convert;
      for (var i = 0; i < arr.length; i++) {
        if (arr[i].svgW > 0 && arr[i].svgH > 0) {
          return { w: arr[i].svgW, h: arr[i].svgH };
        }
      }
      return { w: SVG_FALLBACK_SIZE, h: SVG_FALLBACK_SIZE };
    },

    get svgRenderSize() {
      var n = this.svgNatural();
      var scale = SVG_SCALES[this.forms.image.svg_scale] || 1;
      return { w: Math.round(n.w * scale), h: Math.round(n.h * scale) };
    },

    /* The slider is the only size control for an SVG, so it writes the width
       the job actually carries; height follows the aspect ratio. */
    applySVGScale() {
      if (!this.svgInput) return;
      this.forms.image.width = this.svgRenderSize.w;
      this.forms.image.height = 0;
    },

    get imageOutputIsSVG() {
      var f = this.forms.image.format;
      if (f === "svg" || f === "svg_trace") return true;
      return f === "source" && this.uploadImageFormat("convert") === "svg";
    },

    crfBounds() {
      var codec = this.forms.convert.video_codec;
      var bounds =
        this.presets && this.presets.crf_bounds
          ? this.presets.crf_bounds[codec]
          : null;
      return bounds && bounds.length === 2 ? bounds : null;
    },

    get crfEnabled() {
      return this.crfBounds() !== null;
    },

    /* Each codec has its own quality range; the same number means different
       things across them. */
    clampCRF() {
      var b = this.crfBounds();
      if (!b) return;
      var crf = Number(this.forms.convert.crf);
      if (isNaN(crf)) crf = b[0] + Math.round((b[1] - b[0]) / 2);
      this.forms.convert.crf = Math.min(b[1], Math.max(b[0], crf));
    },

    /* Re-resolve the preset rather than carrying the old codec's numbers. */
    onCodecChange() {
      this.applyPreset();
      this.clampCRF();
      this.errors.convert = {};
      this.formError.convert = "";
    },

    get convertIsCustom() {
      return this.forms.convert.preset === "custom";
    },

    /* Copying the video stream and tuning an encode are mutually exclusive -
       the server rejects the pair, so the option simply is not offered. */
    get convertCodecOptions() {
      var all = this.options("video_codecs");
      if (this.convertIsCustom) return all;
      return all.filter(function (o) {
        return o.value !== "copy";
      });
    },

    /* What a preset resolves to for the current codec, as published by the
       server. Null for "custom", which resolves nothing. */
    presetResolved() {
      var c = this.forms.convert;
      var table = this.presets && this.presets.encode_preset_values;
      var entry = table && table[c.preset];
      if (!entry) return null;
      var crf = entry.crf && entry.crf[c.video_codec];
      return {
        speed: entry.speed || "",
        crf: typeof crf === "number" ? crf : null,
      };
    },

    /* A preset writes into the visible speed and quality fields rather than
       hiding them, so what it chose stays on screen. */
    applyPreset() {
      var r = this.presetResolved();
      if (!r) return;
      var c = this.forms.convert;
      this.setAndFlash("convert", c, "speed", r.speed);
      if (r.crf !== null) this.setAndFlash("convert", c, "crf", r.crf);
    },

    onPresetChange() {
      var c = this.forms.convert;
      var allowed = this.convertCodecOptions;
      var ok = allowed.some(function (o) {
        return o.value === c.video_codec;
      });
      if (!ok && allowed.length) c.video_codec = allowed[0].value;
      this.applyPreset();
      this.clampCRF();
      this.errors.convert = {};
      this.formError.convert = "";
      this.refreshIcons();
    },

    /* Touching either knob means the user has stopped following the preset. */
    onManualEncodeEdit() {
      if (this.convertIsCustom) return;
      var r = this.presetResolved();
      var c = this.forms.convert;
      if (r && c.speed === r.speed && Number(c.crf) === r.crf) return;
      this.forms.convert.preset = "custom";
    },

    get imageQualityIsCustom() {
      return this.forms.image.quality_preset === "custom";
    },

    /* What a quality preset resolves to, as published by the server. Null
       for "custom", which resolves nothing. */
    imagePresetResolved() {
      var table = this.presets && this.presets.image_convert_quality;
      var q = table && table[this.forms.image.quality_preset];
      return typeof q === "number" ? q : null;
    },

    applyImagePreset(silent) {
      var q = this.imagePresetResolved();
      if (q === null) return;
      this.setAndFlash("convert", this.forms.image, "quality", q, silent);
    },

    onImagePresetChange() {
      this.applyImagePreset();
      this.errors.convert = {};
      this.formError.convert = "";
    },

    /* Touching the slider means the user has stopped following the preset. */
    onImageQualityManualEdit() {
      if (this.imageQualityIsCustom) return;
      var q = this.imagePresetResolved();
      if (q !== null && Number(this.forms.image.quality) === q) return;
      this.forms.image.quality_preset = "custom";
    },

    get compressBySize() {
      return this.forms.compress.target === "target_size";
    },

    get imageCompressReady() {
      var t = this.presets && this.presets.image_compress_quality;
      return !!(t && Object.keys(t).length);
    },

    /* Squeezing a still to an exact byte count needs an iterative quality
       search the server does not do, so that target is video-only. */
    compressTargetOptions() {
      var all = this.options("compress_targets");
      if (this.uploadKind("compress") !== "image") return all;
      var table = (this.presets && this.presets.image_compress_quality) || {};
      return all.filter(function (o) {
        return Object.prototype.hasOwnProperty.call(table, o.value);
      });
    },

    /* Shown in the form: picking a target otherwise looks like it does
       nothing, because its effect is invisible. */
    compressResolvedCRF() {
      var f = this.forms.compress;
      var table = this.presets && this.presets.compress_target_values;
      var entry = table && table[f.target];
      var crf = entry && entry.crf && entry.crf[f.video_codec];
      return typeof crf === "number" ? crf : null;
    },

    imageCompressQuality() {
      var table = (this.presets && this.presets.image_compress_quality) || {};
      return table[this.forms.compress.target];
    },

    targetSizeBounds() {
      var b = this.presets && this.presets.target_size_bounds;
      var min = b && Number(b.min_mb);
      var max = b && Number(b.max_mb);
      return {
        min: min && !isNaN(min) ? min : 1,
        max: max && !isNaN(max) ? max : 65536,
      };
    },

    /* Writes into the fields below rather than deciding behind them. */
    applyCompressTarget(silent) {
      var table = this.presets && this.presets.compress_target_values;
      var entry = table && table[this.forms.compress.target];
      if (!entry) return;
      var f = this.forms.compress;
      this.setAndFlash("compress", f, "speed", entry.speed, silent);
      this.setAndFlash("compress", f, "resolution", entry.resolution, silent);
      this.setAndFlash("compress", f, "audio_bitrate", entry.audio_bitrate, silent);
    },

    setAndFlash(form, target, field, value, silent) {
      /* Not !value: 0 is a real value here (the image "original" preset). */
      if (value === undefined || value === null || value === "") return;
      if (String(target[field]) === String(value)) return;
      target[field] = value;
      if (silent) return;
      var seq = (this.flash[form][field] || 0) + 1;
      this.flash[form][field] = seq;
      var self = this;
      setTimeout(function () {
        if (self.flash[form][field] === seq) self.flash[form][field] = 0;
      }, flashMs);
    },

    /* Alternating names: re-applying one mid-animation would not restart it. */
    flashClass(form, field) {
      var seq = this.flash[form][field] || 0;
      if (!seq) return "";
      return seq % 2 ? "flash-a" : "flash-b";
    },

    onTargetChange() {
      this.applyCompressTarget();
      this.errors.compress = {};
      this.formError.compress = "";
      this.refreshIcons();
    },

    get maxUploadBytes() {
      var v = this.presets && Number(this.presets.max_upload_bytes);
      return v && !isNaN(v) ? v : 0;
    },

    async fetchFormats() {
      var url = this.probe.url.trim();
      this.probe.error = "";
      this.errors.download = {};
      this.formError.download = "";
      if (!url) {
        this.errors.download = { url: "Paste a link first." };
        this.refreshIcons();
        return;
      }
      this.probe.busy = true;
      this.probe.result = null;
      this.dl.format_id = "";
      this.dl.showFormats = false;
      this.refreshIcons();
      try {
        this.probe.result = await api("/probe", {
          method: "POST",
          body: { url: url },
        });
      } catch (e) {
        if (e instanceof OfflineError) this.offline = e.message;
        else if (e.field) this.errors.download[e.field] = e.message;
        else this.probe.error = e.message;
      } finally {
        this.probe.busy = false;
        this.refreshIcons();
      }
    },

    get probeFormats() {
      var r = this.probe.result;
      if (!r || !r.formats) return [];
      var wantAudioOnly = this.dl.mode === "audio";
      return r.formats.filter(function (f) {
        return wantAudioOnly ? f.has_audio && !f.has_video : f.has_video;
      });
    },

    /* What the folded table is set to, so collapsing it hides no choice. */
    get formatSummary() {
      var f = this.selectedFormat;
      if (!f) return "best available";
      var parts = [f.id];
      if (f.resolution) parts.push(f.resolution);
      else if (f.ext) parts.push(f.ext);
      if (f.filesize) parts.push(humanBytes(f.filesize));
      return parts.join(" · ");
    },

    get selectedFormat() {
      var id = this.dl.format_id;
      if (!id) return null;
      var list = (this.probe.result && this.probe.result.formats) || [];
      for (var i = 0; i < list.length; i++) {
        if (list[i].id === id) return list[i];
      }
      return null;
    },

    /* High-resolution streams are video-only on every DASH site and the server
       merges the best audio in; say so, because "1080p" next to "Audio: none"
       otherwise reads like a warning. */
    get mergesAudio() {
      var f = this.selectedFormat;
      return this.dl.mode === "video" && !!f && !f.has_audio;
    },

    setMode(mode) {
      this.dl.mode = mode;
      this.dl.format_id = "";
      this.errors.download = {};
      this.refreshIcons();
    },

    onDrop(event, tab) {
      this.dragging[tab] = false;
      var files = event.dataTransfer && event.dataTransfer.files;
      if (files && files.length) this.addFiles(files, tab);
    },

    onPick(event, tab) {
      var files = event.target.files;
      if (files && files.length) this.addFiles(files, tab);
      event.target.value = "";
    },

    /* One batch, one kind: the options on screen belong to either a video or
       an image, so a mixed drop would silently apply the wrong ones and the
       odd files are refused by name instead. Fingerprinting before queueing
       stops the same content being added twice under two names. */
    async addFiles(files, tab) {
      this.formError[tab] = "";
      this.errors[tab] = {};

      var list = Array.prototype.slice.call(files);
      var kind = this.uploadKind(tab) || fileKind(list[0]);
      var wrongKind = [];
      var duplicates = [];

      for (var i = 0; i < list.length; i++) {
        var file = list[i];
        if (fileKind(file) !== kind) {
          wrongKind.push(file.name);
          continue;
        }
        var print = await fileFingerprint(file);
        if (this.hasFingerprint(tab, print)) {
          duplicates.push(file.name);
          continue;
        }
        /* Not awaited: the entry is pushed synchronously, so the next file's
           duplicate check already sees it, and the uploads overlap. */
        this.startUpload(file, tab, print);
      }

      var notes = [];
      if (duplicates.length) {
        notes.push("Already added: " + duplicates.join(", "));
      }
      if (wrongKind.length) {
        notes.push(
          "One batch has to be all videos or all images, so these were left out: " +
            wrongKind.join(", "),
        );
      }
      if (notes.length) this.formError[tab] = notes.join(". ") + ".";

      this.syncKindOptions(tab);
      this.refreshIcons();
    },

    hasFingerprint(tab, print) {
      return this.uploads[tab].some(function (u) {
        return u.print === print;
      });
    },

    async startUpload(file, tab, print) {
      var uid = "u" + ++this.uploadSeq[tab];
      var max = this.maxUploadBytes;

      this.uploads[tab].push({
        uid: uid,
        name: file.name,
        size: file.size,
        progress: 0,
        uploading: false,
        error: "",
        upload_id: "",
        kind: fileKind(file),
        print: print || "",
      });

      var self = this;
      /* Look the entry up by uid every time rather than holding the object:
         only the copy inside the reactive array is the one Alpine watches, and
         the user may have removed it while the bytes were still going up. */
      var live = function () {
        var arr = self.uploads[tab];
        for (var i = 0; i < arr.length; i++) {
          if (arr[i].uid === uid) return arr[i];
        }
        return null;
      };

      if (max && file.size > max) {
        var tooBig = live();
        if (tooBig) {
          tooBig.error =
            "That file is " + humanBytes(file.size) +
            ". The limit is " + humanBytes(max) + ".";
        }
        this.refreshIcons();
        return;
      }

      if (extOf(file.name) === "svg" && typeof file.text === "function") {
        file.text().then(function (text) {
          var size = parseSVGSize(text);
          var u = live();
          if (!size || !u) return;
          u.svgW = size.w;
          u.svgH = size.h;
          self.applySVGScale();
        });
      }

      var starting = live();
      if (starting) starting.uploading = true;
      this.refreshIcons();

      try {
        var res = await uploadFile(file, function (pct) {
          var u = live();
          if (u) u.progress = pct;
        });
        var done = live();
        if (done) {
          done.upload_id = res.upload_id;
          done.name = res.filename || file.name;
          done.size = res.size || file.size;
          done.progress = 100;
        }
      } catch (e) {
        var failed = live();
        if (failed) failed.error = e.message;
        if (e instanceof OfflineError) self.offline = e.message;
      } finally {
        var current = live();
        if (current) current.uploading = false;
        this.refreshIcons();
      }
    },

    async openLibrary(tab) {
      this.library = { open: true, tab: tab, loading: true, error: "", files: [], busy: "" };
      try {
        var res = await api("/files");
        this.library.files = (res && res.files) || [];
      } catch (e) {
        this.library.error = e.message;
        if (e instanceof OfflineError) this.offline = e.message;
      } finally {
        this.library.loading = false;
        this.refreshIcons();
      }
    },

    closeLibrary() {
      this.library.open = false;
    },

    libraryPrint(file) {
      return "job:" + file.job_id + ":" + file.source;
    },

    libraryFiles() {
      var tab = this.library.tab;
      var kind = this.uploadKind(tab);
      var self = this;
      return this.library.files.filter(function (f) {
        if (kind && fileKind({ name: f.name }) !== kind) return false;
        return !self.hasFingerprint(tab, self.libraryPrint(f));
      });
    },

    async pickLibraryFile(file) {
      var tab = this.library.tab;
      this.library.busy = this.libraryPrint(file);
      this.library.error = "";
      try {
        var res = await api("/jobs/" + file.job_id + "/reuse", {
          method: "POST",
          body: { source: file.source },
        });
        this.uploads[tab].push({
          uid: "u" + ++this.uploadSeq[tab],
          name: res.filename || file.name,
          size: res.size || file.size,
          progress: 100,
          uploading: false,
          error: "",
          upload_id: res.upload_id,
          kind: fileKind({ name: res.filename || file.name }),
          print: this.libraryPrint(file),
        });
        this.formError[tab] = "";
        this.errors[tab] = {};
        this.closeLibrary();
        this.syncKindOptions(tab);
      } catch (e) {
        this.library.error = e.message;
        if (e instanceof OfflineError) this.offline = e.message;
      } finally {
        this.library.busy = "";
        this.refreshIcons();
      }
    },

    removeUpload(tab, uid) {
      var arr = this.uploads[tab];
      for (var i = 0; i < arr.length; i++) {
        if (arr[i].uid === uid) {
          arr.splice(i, 1);
          break;
        }
      }
      this.errors[tab] = {};
      this.formError[tab] = "";
      this.syncKindOptions(tab);
      this.refreshIcons();
    },

    clearUploads(tab) {
      this.uploads[tab] = [];
      this.errors[tab] = {};
      this.formError[tab] = "";
      this.refreshIcons();
    },

    readyUploads(tab) {
      return this.uploads[tab].filter(function (u) {
        return u.upload_id && !u.uploading;
      });
    },

    uploadReady(tab) {
      var arr = this.uploads[tab];
      if (!arr.length) return false;
      var busy = arr.some(function (u) {
        return u.uploading;
      });
      return !busy && this.readyUploads(tab).length > 0;
    },

    /* The download tab's equivalent of uploadReady: a link, not a file, is what
       submitDownload needs, and it trims the same way before checking. */
    downloadReady() {
      return this.probe.url.trim().length > 0;
    },

    uploadBusy(tab) {
      return this.uploads[tab].some(function (u) {
        return u.uploading;
      });
    },

    /* "" until a file has been chosen, which is why both tabs show nothing but
       their dropzone to begin with: video and image options share nothing. */
    uploadKind(tab) {
      var arr = this.uploads[tab];
      return (arr.length && arr[0].kind) || "";
    },

    /* Swapping a video for an image can strand a setting the new kind does not
       offer - a size target, say, which stills do not support. */
    syncKindOptions(tab) {
      if (tab === "convert") {
        var formats = this.imageFormatOptions();
        var picked = this.forms.image.format;
        var offered = formats.some(function (o) {
          return o.value === picked;
        });
        if (!offered && formats.length) this.forms.image.format = formats[0].value;
        this.applySVGScale();
        return;
      }
      if (tab !== "compress") return;
      var allowed = this.compressTargetOptions();
      var current = this.forms.compress.target;
      var ok = allowed.some(function (o) {
        return o.value === current;
      });
      if (!ok && allowed.length) this.forms.compress.target = allowed[0].value;
    },

    /* Each branch sends exactly the keys the server allow-lists for that job
       type - it rejects unknown fields outright. */
    downloadParams() {
      var d = this.dl;
      if (d.mode === "audio") {
        return {
          mode: "audio",
          format_id: d.format_id,
          audio_format: d.audio_format,
          audio_bitrate: d.audio_bitrate,
          embed_thumbnail: d.embed_thumbnail,
          embed_metadata: d.embed_metadata,
          retention_days: Number(d.retention_days),
        };
      }
      return {
        mode: "video",
        format_id: d.format_id,
        container: d.container,
        embed_subs: d.embed_subs,
        embed_thumbnail: d.embed_thumbnail,
        embed_metadata: d.embed_metadata,
        retention_days: Number(d.retention_days),
      };
    },

    convertParams() {
      var f = this.forms.convert;
      return {
        container: f.container,
        preset: f.preset,
        video_codec: f.video_codec,
        /* Sent even under a preset, where the server overrides both. */
        speed: f.speed,
        crf: Number(f.crf),
        resolution: f.resolution,
        frame_rate: f.frame_rate,
        audio_codec: f.audio_codec,
        audio_bitrate: f.audio_bitrate,
        strip_metadata: !!f.strip_metadata,
        retention_days: Number(f.retention_days),
      };
    },

    compressParams() {
      var f = this.forms.compress;
      return {
        target: f.target,
        /* The server accepts this only for the target_size target. */
        target_size_mb:
          f.target === "target_size" ? Number(f.target_size_mb) || 0 : 0,
        container: f.container,
        video_codec: f.video_codec,
        speed: f.speed,
        resolution: f.resolution,
        audio_bitrate: f.audio_bitrate,
        strip_metadata: !!f.strip_metadata,
        retention_days: Number(f.retention_days),
      };
    },

    imageParams() {
      var f = this.forms.image;
      return {
        format: f.format,
        quality: Number(f.quality),
        width: Number(f.width) || 0,
        height: Number(f.height) || 0,
        strip_metadata: !!f.strip_metadata,
        retention_days: Number(f.retention_days),
      };
    },

    /* Squeezing an image is the same job as converting one, at a quality the
       server publishes per compress target, so it keeps the input's format. */
    imageCompressParams() {
      var f = this.forms.compress;
      var table = (this.presets && this.presets.image_compress_quality) || {};
      return {
        format: "source",
        quality: Number(table[f.target]) || 0,
        width: Number(f.width) || 0,
        height: Number(f.height) || 0,
        strip_metadata: !!f.strip_metadata,
        retention_days: Number(f.retention_days),
      };
    },

    /* A tab submits whichever job type its file calls for. */
    wireType(tab) {
      if (tab === "download") return "download";
      return this.uploadKind(tab) === "image" ? "image" : tab;
    },

    async submit(tab) {
      if (this.submitting) return;
      this.errors[tab] = {};
      this.formError[tab] = "";

      this.submitting = tab;
      this.refreshIcons();
      try {
        if (tab === "download") {
          await this.submitDownload();
        } else {
          await this.submitFiles(tab);
        }
      } finally {
        this.submitting = "";
        this.refreshIcons();
      }
    },

    async submitDownload() {
      var url = this.probe.url.trim();
      if (!url) {
        this.errors.download = { url: "Paste a link first." };
        return;
      }
      try {
        var job = await api("/jobs", {
          method: "POST",
          body: { type: "download", url: url, params: this.downloadParams() },
        });
        this.upsertJob(job);
        this.enqueue(job.id);
        this.announce =
          "Download queued: " + (job.title || job.source || job.id);
        this.probe.url = "";
        this.probe.result = null;
        this.dl.format_id = "";
      } catch (e) {
        this.reportSubmitError("download", e);
      }
    },

    /* One job per file, sharing the settings on screen. Submitted in sequence:
       a failure part-way through leaves the files it did not reach listed. */
    async submitFiles(tab) {
      var ready = this.readyUploads(tab);
      if (!ready.length) {
        this.formError[tab] = this.uploadBusy(tab)
          ? "Still uploading - one moment."
          : "Add a file first.";
        return;
      }

      var isImage = this.uploadKind(tab) === "image";
      if (isImage && tab === "compress" && !this.imageCompressReady) {
        this.formError[tab] =
          "This server has not published image compression settings.";
        return;
      }

      var type = this.wireType(tab);
      var params;
      if (isImage) {
        params =
          tab === "compress" ? this.imageCompressParams() : this.imageParams();
      } else {
        params =
          tab === "convert" ? this.convertParams() : this.compressParams();
      }

      var started = 0;
      for (var i = 0; i < ready.length; i++) {
        var entry = ready[i];
        try {
          var job = await api("/jobs", {
            method: "POST",
            body: { type: type, upload_id: entry.upload_id, params: params },
          });
          this.upsertJob(job);
          this.enqueue(job.id);
          this.removeUpload(tab, entry.uid);
          started++;
        } catch (e) {
          this.reportSubmitError(tab, e);
          break; // the same settings would fail for every remaining file
        }
      }

      if (started) {
        this.announce =
          started === 1
            ? TYPE_LABEL[type] + " job queued."
            : started + " " + TYPE_LABEL[type] + " jobs queued.";
      }
    },

    reportSubmitError(tab, e) {
      if (e instanceof OfflineError) this.offline = e.message;
      else if (e.field) this.errors[tab][e.field] = e.message;
      else this.formError[tab] = e.message;
    },

    /* This replaces the whole list, and reconnects call it automatically, so a
       response in flight when a job is deleted routinely races the delete.
       Two guards: a sequence number retires a stale response, and tombstones
       stop a snapshot taken before the delete from resurrecting the row. */
    async loadJobs() {
      this.jobsLoading = true;
      var seq = ++this.jobsSeq;
      try {
        var res = await api("/jobs?limit=500");
        if (seq !== this.jobsSeq) return; // a newer load, or a delete, won
        var self = this;
        this.jobs = ((res && res.jobs) || []).filter(function (j) {
          return !self.recentlyDeleted[j.id];
        });
        this.sortJobs();
        this.pruneTombstones();
        this.now = Date.now();
        this.offline = "";
      } catch (e) {
        if (e instanceof OfflineError) this.offline = e.message;
      } finally {
        if (seq === this.jobsSeq) this.jobsLoading = false;
        this.refreshIcons();
      }
    },

    /* A tombstone only has to outlive requests already in flight. */
    pruneTombstones() {
      var cutoff = Date.now() - 60000;
      for (var id in this.recentlyDeleted) {
        if (this.recentlyDeleted[id] < cutoff) delete this.recentlyDeleted[id];
      }
    },

    sortJobs() {
      this.jobs.sort(function (a, b) {
        return (
          new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
        );
      });
    },

    /* Returns true when the job was new to the list, so callers know whether
       fresh icon nodes need drawing. */
    upsertJob(job) {
      if (!job || !job.id) return false;
      /* A status event can race a delete; the delete is the later intent. */
      if (this.recentlyDeleted[job.id]) return false;
      var idx = this.jobs.findIndex(function (j) {
        return j.id === job.id;
      });
      if (idx !== -1 && STATUS_RANK[job.status] < STATUS_RANK[this.jobs[idx].status]) {
        return false;
      }
      if (idx === -1) this.jobs.unshift(job);
      else this.jobs.splice(idx, 1, job);
      this.sortJobs();
      return idx === -1;
    },

    get visibleJobs() {
      var filter = this.jobFilter;
      if (!filter || filter === "all") return this.jobs;
      return this.jobs.filter(function (j) {
        return j.status === filter;
      });
    },

    get jobPages() {
      return Math.max(1, Math.ceil(this.visibleJobs.length / JOBS_PER_PAGE));
    },

    /* A page that a filter change or a deletion emptied falls back to the last
       one that still has rows. */
    get pagedJobs() {
      var page = Math.min(this.jobPage, this.jobPages);
      var start = (page - 1) * JOBS_PER_PAGE;
      return this.visibleJobs.slice(start, start + JOBS_PER_PAGE);
    },

    get jobRange() {
      var total = this.visibleJobs.length;
      if (!total) return "";
      var page = Math.min(this.jobPage, this.jobPages);
      var start = (page - 1) * JOBS_PER_PAGE;
      return start + 1 + "-" + Math.min(start + JOBS_PER_PAGE, total) + " of " + total;
    },

    goToJobPage(page) {
      this.jobPage = Math.min(Math.max(1, page), this.jobPages);
      this.refreshIcons();
    },

    /* Ids only: a job the server dropped stops counting the moment its row is
       gone, without a second list to keep in step. */
    get pickedJobs() {
      var picked = this.picked;
      return this.jobs.filter(function (j) {
        return picked[j.id];
      });
    },

    get pageAllPicked() {
      var page = this.pagedJobs;
      var picked = this.picked;
      return (
        page.length > 0 &&
        page.every(function (j) {
          return picked[j.id];
        })
      );
    },

    togglePage() {
      var on = !this.pageAllPicked;
      var self = this;
      this.pagedJobs.forEach(function (j) {
        if (on) self.picked[j.id] = true;
        else delete self.picked[j.id];
      });
      this.refreshIcons();
    },

    togglePick(id) {
      if (this.picked[id]) delete this.picked[id];
      else this.picked[id] = true;
    },

    clearPicked() {
      this.picked = {};
      this.refreshIcons();
    },

    async deletePicked() {
      var jobs = this.pickedJobs;
      if (!jobs.length || this.bulkBusy) return;
      this.bulkBusy = true;
      for (var i = 0; i < jobs.length; i++) {
        await this.deleteJob(jobs[i]);
        delete this.picked[jobs[i].id];
      }
      this.bulkBusy = false;
      this.announce = "Deleted " + jobs.length + " jobs.";
      this.refreshIcons();
    },

    async cancelPicked() {
      var jobs = this.pickedJobs.filter(isActive);
      if (!jobs.length || this.bulkBusy) return;
      this.bulkBusy = true;
      for (var i = 0; i < jobs.length; i++) {
        await this.cancelJob(jobs[i]);
      }
      this.bulkBusy = false;
      this.refreshIcons();
    },

    get pickedActive() {
      return this.pickedJobs.filter(isActive).length;
    },

    get runningCount() {
      return this.jobs.filter(isActive).length;
    },

    enqueue(id) {
      if (!id) return;
      if (this.queue.indexOf(id) === -1) this.queue.unshift(id);
      /* The docked panel eats real space on a phone; leave it collapsed to
         the pill there and let the user open it. */
      if (!isMobileViewport()) this.queueOpen = true;
      this.refreshIcons();
    },

    get queueJobs() {
      var byId = {};
      for (var i = 0; i < this.jobs.length; i++) byId[this.jobs[i].id] = this.jobs[i];
      var out = [];
      for (var j = 0; j < this.queue.length; j++) {
        var job = byId[this.queue[j]];
        if (job) out.push(job);
      }
      return out;
    },

    get queueRunning() {
      return this.queueJobs.filter(isActive).length;
    },

    /* Percentage across the whole batch, so the collapsed pill can show one
       number for "12 files" without the user opening the panel. */
    get queuePercent() {
      var list = this.queueJobs;
      if (!list.length) return 0;
      var total = 0;
      for (var i = 0; i < list.length; i++) {
        var p = this.jobSettled(list[i]) ? 100 : this.jobProgress(list[i]);
        total += p < 0 ? 0 : Math.min(100, p);
      }
      return Math.round(total / list.length);
    },

    dismissFromQueue(id) {
      var i = this.queue.indexOf(id);
      if (i !== -1) this.queue.splice(i, 1);
      if (!this.queue.length) this.queueOpen = false;
      this.refreshIcons();
    },

    toggleQueue() {
      this.queueOpen = !this.queueOpen;
      this.refreshIcons();
    },

    jobSettled(job) {
      return !!job && !isActive(job);
    },

    /* Once a job has produced a file, that file's name is what identifies it;
       the name it started from moves to the line below. */
    jobName(job) {
      return (job && (job.output_name || job.title || job.source || job.id)) || "";
    },

    jobOrigin(job) {
      if (!job || !job.output_name) return "";
      var from = job.source || job.title || "";
      return from === job.output_name ? "" : from;
    },

    queueStatusLine(job) {
      if (job.status === "done") {
        return job.output_size ? humanBytes(job.output_size) : "done";
      }
      if (job.status === "failed") return job.error || "failed";
      if (job.status === "canceled" || job.status === "expired") return job.status;
      var pct = this.jobProgress(job);
      var speed = this.jobSpeed(job);
      var parts = [this.jobStage(job) || job.status];
      if (pct >= 0) parts.push(Math.round(pct) + "%");
      if (speed) parts.push(speed);
      return parts.join(" · ");
    },

    jobProgress(job) {
      var l = this.live[job.id];
      if (l && typeof l.percent === "number") return l.percent;
      if (job.status === "done") return 100;
      return typeof job.progress === "number" ? job.progress : -1;
    },

    jobStage(job) {
      var l = this.live[job.id];
      return (l && l.stage) || job.stage || "";
    },

    jobSpeed(job) {
      var l = this.live[job.id];
      return (l && l.speed) || "";
    },

    jobEta(job) {
      var l = this.live[job.id];
      return (l && l.eta) || "";
    },

    isIndeterminate(job) {
      return job.status === "running" && this.jobProgress(job) < 0;
    },

    percentLabel(job) {
      var p = this.jobProgress(job);
      if (job.status !== "running") return "";
      if (p < 0) return "working";
      return Math.round(p) + "%";
    },

    railWidth(job) {
      if (job.status !== "running") return "";
      var p = this.jobProgress(job);
      return (p < 0 ? 0 : Math.min(100, p)) + "%";
    },

    typeIcon(type) {
      return TYPE_ICON[type] || "file";
    },

    typeLabel(type) {
      return TYPE_LABEL[type] || type;
    },

    downloadUrl(job) {
      return "/api/jobs/" + encodeURIComponent(job.id) + "/download";
    },

    /* Every finished job is media - image, audio or video - so the extension
       alone is enough to pick the right tag; anything unrecognized falls
       back to video since that is the broader player. */
    mediaKind(job) {
      var ext = extOf(job && job.output_name);
      if (IMAGE_EXTS.indexOf(ext) !== -1) return "image";
      if (AUDIO_EXTS.indexOf(ext) !== -1) return "audio";
      return "video";
    },

    canPreview(job) {
      return job.status === "done" && !!job.output_name;
    },

    openPreview(job) {
      this.preview = { job: job, kind: this.mediaKind(job), url: this.downloadUrl(job) };
    },

    closePreview() {
      this.preview = null;
    },

    /* Only where a plain <a download> would strand the user: see isStandalone.
       Everywhere else the anchor is left alone. */
    managedSave() {
      return isStandalone() && isIOS();
    },

    downloadJob(job, ev) {
      if (!this.managedSave()) return; // let the anchor do its ordinary job
      if (ev) ev.preventDefault();
      this.saveFile(job);
    },

    async saveFile(job) {
      var self = this;
      var name = job.output_name || job.title || "download";
      var url = this.downloadUrl(job);

      /* Too big to hold in memory, but a browser window can still save it. */
      if (Number(job.output_size) > SAVE_BLOB_MAX) {
        this.openOutside(url);
        return;
      }

      this.cancelSave();
      var tick = ++saveTick;
      this.save = { id: job.id, name: name, url: url, pct: 0, phase: "fetching", error: "" };

      var req = fetchBlob(url, function (pct) {
        if (tick === saveTick) self.save.pct = pct;
      });
      saveRequest = req;

      var blob;
      try {
        blob = await req.done;
      } catch (e) {
        if (tick !== saveTick) return; // superseded; its card is not ours
        saveRequest = null;
        /* Canceled here, or already on the way to the login page. */
        if (e.status === 0 || e.status === 401) {
          this.save.phase = "";
          return;
        }
        this.save.phase = "error";
        this.save.error = e.message || "The file could not be fetched.";
        return;
      }
      if (tick !== saveTick) return;
      saveRequest = null;

      savePending = new File([blob], name, {
        type: blob.type || "application/octet-stream",
      });
      this.save.phase = "ready";
      /* The share sheet wants a fresh user gesture and the fetch has probably
         outlived the tap that started it. Try anyway - a short fetch usually
         still counts - and leave a Save button when the system refuses. */
      await this.handOff(true);
    },

    /* `auto` marks the attempt made straight after the fetch, where a refused
       gesture is expected and must not read as an error. */
    async handOff(auto) {
      var file = savePending;
      if (!file) return;

      var payload = { files: [file], title: file.name };
      var sharable =
        navigator.canShare && navigator.share && navigator.canShare(payload);
      /* No file sharing on old iOS, or on plain HTTP where it is undefined. */
      if (!sharable) {
        if (!auto) this.saveViaLink(file);
        return;
      }

      try {
        await navigator.share(payload);
        this.endSave("Saved " + file.name + ".");
      } catch (e) {
        /* Dismissing the sheet is the user's call, so the card goes with it;
           any other failure is the gesture, which the Save button retries. */
        if (e && e.name === "AbortError") this.endSave("");
        else if (!auto) this.saveViaLink(file);
      }
    },

    saveViaLink(file) {
      var url = URL.createObjectURL(file);
      var a = document.createElement("a");
      a.href = url;
      a.download = file.name;
      a.rel = "noopener";
      document.body.appendChild(a);
      a.click();
      a.remove();
      /* Revoking straight away can outrun the save still reading the URL. */
      setTimeout(function () {
        URL.revokeObjectURL(url);
      }, 60000);
      this.endSave("Saved " + file.name + ".");
    },

    /* The escape hatch: a separate window, which on iOS is the in-app browser
       with its own Done button. */
    openOutside(url) {
      window.open(url || this.save.url, "_blank", "noopener");
      this.endSave("");
    },

    cancelSave() {
      var req = saveRequest;
      saveRequest = null;
      if (req) req.xhr.abort();
      this.endSave("");
    },

    endSave(message) {
      savePending = null;
      this.save = emptySave();
      if (message) this.announce = message;
      this.refreshIcons();
    },

    /* The row button doubles as the progress readout for its own save. */
    saveLabel(job) {
      if (this.save.id === job.id && this.save.phase === "fetching") {
        return this.save.pct + "%";
      }
      return "Download";
    },

    async cancelJob(job) {
      try {
        await api("/jobs/" + encodeURIComponent(job.id) + "/cancel", {
          method: "POST",
        });
        this.announce = "Canceling " + (job.title || job.id) + ".";
      } catch (e) {
        if (e instanceof OfflineError) this.offline = e.message;
        else this.jobsError = "Could not cancel that job - " + e.message;
      }
      this.refreshIcons();
    },

    async deleteJob(job) {
      var id = job && job.id;
      if (!id || this.deleting[id]) return;
      this.deleting[id] = true;
      this.jobsError = "";
      try {
        await api("/jobs/" + encodeURIComponent(id), { method: "DELETE" });
        this.dropJob(id);
        this.announce = "Deleted " + (job.title || id) + ".";
      } catch (e) {
        if (e instanceof OfflineError) {
          this.offline = e.message;
        } else if (e.status === 404) {
          // Already gone on the server; the row is the only thing left to fix.
          this.dropJob(id);
        } else {
          /* Not `offline`: that banner says the server is unreachable, which
             turned a failed delete into a message about something else. */
          this.jobsError = "Could not delete that job - " + e.message;
        }
      } finally {
        delete this.deleting[id];
        this.refreshIcons();
      }
    },

    /* The index is resolved here rather than before the request: SSE events
       re-sort and mutate the list while a delete is in flight, so an index
       captured earlier can point at a different job by the time it lands. */
    dropJob(id) {
      if (!id) return;
      delete this.picked[id];
      this.recentlyDeleted[id] = Date.now();
      /* Invalidate any list load that straddles this delete. */
      this.jobsSeq++;
      var idx = this.jobs.findIndex(function (j) {
        return j.id === id;
      });
      if (idx !== -1) this.jobs.splice(idx, 1);
      delete this.live[id];
      this.dismissFromQueue(id);
      if (this.preview && this.preview.job.id === id) this.closePreview();
    },

    /* Back in the foreground. A suspended app keeps an EventSource that still
       reports itself open and will never fire again, so more than a glance
       away counts as a dead stream rather than being trusted. Fires on the
       foreground event, not on a timer. */
    resume(hiddenAt) {
      if (!this.presets) {
        this.bootstrap(); // the first load never finished
        return;
      }
      var away = hiddenAt ? Date.now() - hiddenAt : Infinity;
      var dead = !this.es || this.es.readyState === 2 || away > 15000;
      if (!dead) return;

      this.backoff = 1000;
      this.connectEvents(); // clears any pending reconnect itself
      /* connectEvents reconciles only on a reconnect it saw through; a
         first-ever open would leave whatever landed while away unseen. */
      if (!this.everLinked) this.loadJobs();
    },

    connectEvents() {
      var self = this;
      if (this.es) {
        this.es.close();
        this.es = null;
      }
      if (this.reconnectTimer) {
        clearTimeout(this.reconnectTimer);
        this.reconnectTimer = null;
      }

      var es;
      try {
        es = new EventSource("/api/events", { withCredentials: true });
      } catch (e) {
        this.scheduleReconnect();
        return;
      }
      this.es = es;

      es.onopen = function () {
        self.linked = true;
        self.backoff = 1000;
        self.offline = "";
        /* Reconcile once per reconnection: whatever happened while the stream
           was down is picked up here, not by polling. */
        if (self.everLinked) self.loadJobs();
        self.everLinked = true;
        self.refreshIcons();
      };

      var handle = function (event) {
        self.linked = true;
        var msg;
        try {
          msg = JSON.parse(event.data);
        } catch (e) {
          return;
        }
        self.applyEvent(msg);
      };

      ["ready", "created", "progress", "status", "deleted"].forEach(function (
        name,
      ) {
        es.addEventListener(name, handle);
      });

      es.onerror = function () {
        self.linked = false;
        es.close();
        if (self.es === es) self.es = null;
        self.scheduleReconnect();
        self.refreshIcons();
      };
    },

    /* 1s doubling to a 30s cap. No fixed-interval polling. */
    scheduleReconnect() {
      var self = this;
      if (this.reconnectTimer) return;
      var delay = this.backoff;
      this.backoff = Math.min(30000, Math.round(this.backoff * 2));
      this.reconnectTimer = setTimeout(function () {
        self.reconnectTimer = null;
        self.connectEvents();
      }, delay);
    },

    applyEvent(msg) {
      if (!msg || !msg.kind) return;
      this.now = Date.now();

      if (msg.kind === "ready") {
        this.refreshIcons();
        return;
      }

      if (msg.kind === "deleted") {
        this.dropJob(msg.job_id);
        this.refreshIcons();
        return;
      }

      if (msg.kind === "progress" && msg.progress) {
        this.live[msg.job_id] = msg.progress;
        if (msg.job && this.upsertJob(msg.job)) this.refreshIcons();
        return;
      }

      if (msg.job) {
        this.upsertJob(msg.job);
        if (msg.job.status && msg.job.status !== "running") {
          delete this.live[msg.job.id];
        }
        if (msg.kind === "status") {
          this.announce =
            (msg.job.title || msg.job.output_name || msg.job.id) +
            " is " +
            msg.job.status +
            ".";
        }
        this.refreshIcons();
      }
    },

    get linkLabel() {
      if (this.linked) return "live";
      return this.everLinked ? "reconnecting" : "connecting";
    },

    setTheme(choice) {
      this.theme = choice;
      try {
        if (choice === "system") localStorage.removeItem(THEME_KEY);
        else localStorage.setItem(THEME_KEY, choice);
      } catch (e) {
        /* storage unavailable - the choice still applies this session */
      }
      applyTheme(choice);
      this.refreshIcons();
    },

    setFinishSound(on) {
      this.finishSound = !!on;
      try {
        localStorage.setItem(SOUND_KEY, this.finishSound ? "on" : "off");
      } catch (e) {
        /* storage unavailable - the choice still applies this session */
      }
      /* Turning it on is a gesture, so this both previews the sound and buys
         the autoplay permission the background play will need later. */
      if (this.finishSound) playFinishSound();
    },

    async logout() {
      try {
        await api("/auth/logout", { method: "POST", noRedirect: true });
      } catch (e) {
        /* signing out locally regardless */
      }
      if (this.es) this.es.close();
      window.location.assign("/login.html");
    },

    bytes: humanBytes,
    duration: humanDuration,
    absolute: absoluteTime,

    ago(iso) {
      return relativeTime(iso, this.now);
    },

    retentionLabel(job) {
      if (!job.expires_at) return "kept";
      return "expires " + relativeTime(job.expires_at, this.now);
    },
  };
}

function loginForm() {
  return {
    username: "",
    password: "",
    busy: false,
    error: "",
    checking: true,

    async init() {
      applyTheme(storedTheme());
      try {
        var me = await api("/auth/me", { noRedirect: true });
        if (me && (me.authenticated || me.auth_required === false)) {
          window.location.assign("/");
          return;
        }
      } catch (e) {
        if (e instanceof OfflineError) {
          this.error = "Cannot reach the server.";
        }
      }
      this.checking = false;
      this.$nextTick(function () {
        drawIcons();
      });
    },

    async signIn() {
      if (this.busy) return;
      this.error = "";
      if (!this.username || !this.password) {
        this.error = "Enter your username and password.";
        this.$nextTick(drawIcons);
        return;
      }
      this.busy = true;
      try {
        await api("/auth/login", {
          method: "POST",
          noRedirect: true,
          body: { username: this.username, password: this.password },
        });
        window.location.assign("/");
      } catch (e) {
        if (e instanceof OfflineError) {
          this.error = "Cannot reach the server.";
        } else if (e.status === 429) {
          this.error = "Too many attempts. Wait a minute, then try again.";
        } else if (e.status === 401) {
          this.error = "That username and password do not match.";
        } else {
          this.error = e.message || "Sign-in failed.";
        }
        this.password = "";
        this.busy = false;
        this.$nextTick(drawIcons);
      }
    },
  };
}

document.addEventListener("alpine:init", function () {
  window.Alpine.data("mediaApp", mediaApp);
  window.Alpine.data("loginForm", loginForm);
});

document.addEventListener("DOMContentLoaded", function () {
  drawIcons();
});
