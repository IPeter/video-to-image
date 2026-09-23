/** Minimal scriptable XMLHttpRequest double. */
export class FakeXhr {
  static last: FakeXhr | null = null;

  method = "";
  url = "";
  headers: Record<string, string> = {};
  body: FormData | null = null;
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
  }
  open(method: string, url: string) {
    this.method = method;
    this.url = url;
  }
  setRequestHeader(name: string, value: string) {
    this.headers[name] = value;
  }
  send(body: FormData) {
    this.body = body;
  }
  getResponseHeader(name: string): string | null {
    const key = Object.keys(this.responseHeaders).find((k) => k.toLowerCase() === name.toLowerCase());
    return key === undefined ? null : this.responseHeaders[key];
  }
  abort() {
    this.aborted = true;
    this.onabort?.();
  }

  // --- test controls ---
  progress(loaded: number, total: number, lengthComputable = true) {
    this.upload.onprogress?.({ loaded, total, lengthComputable } as ProgressEvent);
  }
  respond(status: number, body: unknown, headers: Record<string, string> = {}) {
    this.status = status;
    this.responseHeaders = headers;
    this.responseText = typeof body === "string" ? body : JSON.stringify(body);
    this.onload?.();
  }
  networkError() {
    this.status = 0;
    this.onerror?.();
  }
}
