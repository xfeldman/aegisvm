/**
 * Tether frame helpers — send/receive tether frames via HTTP to the harness.
 *
 * The harness (PID 1) listens on 127.0.0.1:7777 for egress frames.
 * The channel extension listens on 127.0.0.1:7778 for ingress frames.
 */

const HARNESS_URL = "http://127.0.0.1:7777/v1/tether/send";

/** Tether frame envelope — matches the wire format used by aegis-agent and aegis-gateway. */
export interface TetherFrame {
  v: number;
  type: string;
  ts?: string;
  session: {
    channel: string;
    id: string;
  };
  msg_id?: string;
  seq?: number;
  payload?: any;
}

/** Image ref in tether payload — references a blob in /workspace/.aegis/blobs/ */
export interface ImageRef {
  media_type: string;
  blob: string;
  size: number;
}

/** User message payload from tether ingress. */
export interface UserMessagePayload {
  text: string;
  images?: ImageRef[];
  user?: {
    id?: string;
    name?: string;
    username?: string;
  };
}

/** Send a tether frame to the harness for egress. */
export async function sendFrame(frame: TetherFrame): Promise<void> {
  try {
    await fetch(HARNESS_URL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(frame),
    });
  } catch (err) {
    console.error("[aegis-channel] send frame error:", err);
  }
}

/** Send a status.presence frame (typing indicator, tool status). */
export async function sendPresence(
  session: TetherFrame["session"],
  state: string
): Promise<void> {
  await sendFrame({
    v: 1,
    type: "status.presence",
    ts: new Date().toISOString(),
    session,
    payload: { state },
  });
}

/** Send an assistant.delta frame (streaming text chunk). */
export async function sendDelta(
  session: TetherFrame["session"],
  text: string
): Promise<void> {
  await sendFrame({
    v: 1,
    type: "assistant.delta",
    ts: new Date().toISOString(),
    session,
    payload: { text },
  });
}

/** Send an assistant.done frame (complete response). */
export async function sendDone(
  session: TetherFrame["session"],
  text: string,
  images?: ImageRef[]
): Promise<void> {
  const payload: any = { text };
  if (images && images.length > 0) {
    payload.images = images;
  }
  await sendFrame({
    v: 1,
    type: "assistant.done",
    ts: new Date().toISOString(),
    session,
    payload,
  });
}

/** Send a status.ready frame — signals the harness to start draining buffered frames. */
export async function sendReady(): Promise<void> {
  await sendFrame({
    v: 1,
    type: "status.ready",
    ts: new Date().toISOString(),
    session: { channel: "system", id: "boot" },
    payload: { state: "ready" },
  });
}

/**
 * Read a blob from the workspace blob store.
 * Returns raw bytes or null if missing.
 */
export async function readBlob(blobKey: string): Promise<Buffer | null> {
  const { readFile } = await import("node:fs/promises");
  const path = `/workspace/.aegis/blobs/${blobKey}`;
  try {
    return await readFile(path);
  } catch {
    return null;
  }
}

/**
 * Write a blob to the workspace blob store.
 * Returns the content-addressed key.
 */
export async function writeBlob(
  data: Buffer,
  mediaType: string
): Promise<string> {
  const { createHash } = await import("node:crypto");
  const { writeFile, mkdir } = await import("node:fs/promises");

  const hash = createHash("sha256").update(data).digest("hex");
  const ext = extForMediaType(mediaType);
  const key = `${hash}${ext}`;
  const dir = "/workspace/.aegis/blobs";
  const path = `${dir}/${key}`;

  await mkdir(dir, { recursive: true });
  await writeFile(path, data);
  return key;
}

function extForMediaType(mediaType: string): string {
  switch (mediaType) {
    case "image/png":
      return ".png";
    case "image/jpeg":
      return ".jpg";
    case "image/gif":
      return ".gif";
    case "image/webp":
      return ".webp";
    default:
      return ".bin";
  }
}
