/** Minimal scriptable XMLHttpRequest double. Every instance is recorded in `FakeXhr.all`. */
export class FakeXhr {
  static last: FakeXhr | null = null;
  static all: FakeXhr[] = [];

  /** Forgets every recorded request (call in beforeEach). */
  static reset() {
    FakeXhr.last = null;
    FakeXhr.all = [];
  }

  /** Sent requests, oldest first, optionally filtered by method and a URL substring or pattern. */
  static requests(method?: string, url?: string | RegExp): FakeXhr[] {
    return FakeXhr.all.filter(
      (x) =>
        x.sent &&
        (method === undefined || x.method === method) &&
        (url === undefined || (typeof url === "string" ? x.url.includes(url) : url.test(x.url))),
    );
  }

  /** Sent requests that have not ended yet (no answer, error, timeout or abort). */
  static pending(method?: string, url?: string | RegExp): FakeXhr[] {
    return FakeXhr.requests(method, url).filter((x) => !x.done);
  }

  method = "";
  url = "";
  headers: Record<string, string> = {};
  body: Document | XMLHttpRequestBodyInit | null = null;
  sent = false;
  /** True once the request ended in any way. */
  done = false;
  status = 0;
  responseText = "";
  responseHeaders: Record<string, string> = {};
  responseType = "";
  timeout = 0;
  aborted = false;
  upload: { onprogress: ((e: ProgressEvent) => void) | null } = { onprogress: null };
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onabort: (() => void) | null = null;
  ontimeout: (() => void) | null = null;

  constructor() {
    FakeXhr.last = this;
    FakeXhr.all.push(this);
  }
  open(method: string, url: string) {
    this.method = method;
    this.url = url;
  }
  setRequestHeader(name: string, value: string) {
    this.headers[name] = value;
  }
  send(body: Document | XMLHttpRequestBodyInit | null = null) {
    this.body = body;
    this.sent = true;
  }
  getResponseHeader(name: string): string | null {
    const key = Object.keys(this.responseHeaders).find((k) => k.toLowerCase() === name.toLowerCase());
    return key === undefined ? null : this.responseHeaders[key];
  }
  abort() {
    this.aborted = true;
    if (this.done) return;
    this.done = true;
    this.onabort?.();
  }

  // --- test controls ---
  /** The request body as a Blob (chunk PUTs). */
  get blob(): Blob {
    if (!(this.body instanceof Blob)) throw new Error(`${this.method} ${this.url}: body is not a Blob`);
    return this.body;
  }
  /** The request body parsed as JSON (create requests). */
  get json(): unknown {
    if (typeof this.body !== "string") throw new Error(`${this.method} ${this.url}: body is not a string`);
    return JSON.parse(this.body);
  }
  progress(loaded: number, total: number, lengthComputable = true) {
    this.upload.onprogress?.({ loaded, total, lengthComputable } as ProgressEvent);
  }
  respond(status: number, body: unknown = "", headers: Record<string, string> = {}) {
    this.done = true;
    this.status = status;
    this.responseHeaders = headers;
    this.responseText = typeof body === "string" ? body : JSON.stringify(body);
    this.onload?.();
  }
  networkError() {
    this.done = true;
    this.status = 0;
    this.onerror?.();
  }
  timeoutNow() {
    this.done = true;
    this.ontimeout?.();
  }
}
