/**
 * @aegis/openclaw-channel-aegis — AegisVM tether channel extension for OpenClaw.
 *
 * Bridges the AegisVM tether protocol to OpenClaw's channel system.
 * Runs in-process with the OpenClaw gateway — no separate process, no WebSocket hop.
 *
 * Ingress: HTTP server on :7778 receives tether frames from the harness,
 *          normalizes to OpenClaw InboundContext, dispatches to auto-reply.
 * Egress:  Receives OpenClaw responses via channel delivery callback,
 *          emits tether assistant.delta / assistant.done frames.
 */

import { createServer, IncomingMessage, ServerResponse } from "node:http";
import {
  TetherFrame,
  UserMessagePayload,
  sendPresence,
  sendDelta,
  sendDone,
  sendReady,
  readBlob,
  writeBlob,
  ImageRef,
} from "./tether";

const LISTEN_PORT = 7778;

/**
 * OpenClaw plugin registration entry point.
 * Called by the OpenClaw gateway at startup when this extension is discovered.
 */
export default function register(api: any): void {
  console.log("[aegis-channel] registering AegisVM tether channel");

  // Store reference to the gateway API for dispatching messages
  const channel = new AegisChannel(api);

  api.registerChannel({
    id: "aegis",
    meta: {
      id: "aegis",
      label: "AegisVM Tether",
      selectionLabel: "AegisVM",
      docsPath: "",
      blurb: "Bridges AegisVM tether protocol to OpenClaw",
    },
    capabilities: {
      chatTypes: ["dm", "group"],
      media: true,
    },
    config: {
      listAccountIds: async () => ["default"],
      resolveAccount: async (_accountId: string) => ({
        accountId: "default",
        enabled: true,
      }),
    },
    outbound: {
      deliveryMode: "sync",
      sendText: (message: any) => channel.handleOutbound(message),
    },
  });

  // Start the tether HTTP server
  channel.start();
}

/**
 * AegisChannel implements the bridge between tether and OpenClaw.
 */
class AegisChannel {
  private api: any;
  private activeSessions = new Map<
    string,
    { channel: string; id: string }
  >();

  constructor(api: any) {
    this.api = api;
  }

  /** Start the HTTP server for tether frame ingress. */
  start(): void {
    const server = createServer((req, res) => {
      if (req.method === "POST" && req.url === "/v1/tether/recv") {
        this.handleTetherRecv(req, res);
      } else {
        res.writeHead(404);
        res.end();
      }
    });

    server.listen(LISTEN_PORT, "127.0.0.1", () => {
      console.log(`[aegis-channel] listening on :${LISTEN_PORT}`);
      // Signal readiness to harness — it can now drain buffered frames
      sendReady().catch(() => {});
    });
  }

  /** Handle an incoming tether frame from the harness. */
  private async handleTetherRecv(
    req: IncomingMessage,
    res: ServerResponse
  ): Promise<void> {
    const body = await readBody(req);
    res.writeHead(202);
    res.end();

    let frame: TetherFrame;
    try {
      frame = JSON.parse(body);
    } catch {
      console.error("[aegis-channel] invalid frame JSON");
      return;
    }

    if (frame.type === "user.message") {
      this.handleUserMessage(frame).catch((err) => {
        console.error("[aegis-channel] handleUserMessage error:", err);
      });
    }
  }

  /** Process an incoming user message: normalize and dispatch to OpenClaw. */
  private async handleUserMessage(frame: TetherFrame): Promise<void> {
    const payload = frame.payload as UserMessagePayload;
    if (!payload) return;

    const session = frame.session;
    const sessionKey = `${session.channel}:${session.id}`;

    // Track the tether session for egress routing
    this.activeSessions.set(sessionKey, session);

    // Build envelope header (OpenClaw convention for channel messages)
    const channelLabel =
      session.channel.charAt(0).toUpperCase() + session.channel.slice(1);
    const senderName = payload.user?.name || payload.user?.username || "User";
    const senderUsername = payload.user?.username || "";
    const timestamp = frame.ts || new Date().toISOString();

    let envelopeHeader = `[${channelLabel} id:${session.id} ${timestamp}]`;
    if (senderUsername) {
      envelopeHeader += `\nSender: ${senderName} (@${senderUsername})`;
    } else {
      envelopeHeader += `\nSender: ${senderName}`;
    }

    const messageText = payload.text || "";
    const fullBody = envelopeHeader + "\n\n" + messageText;

    // Resolve image attachments from blob store
    const attachments: any[] = [];
    if (payload.images && payload.images.length > 0) {
      for (const img of payload.images) {
        const data = await readBlob(img.blob);
        if (data) {
          attachments.push({
            type: "image",
            mimeType: img.media_type,
            data,
            filename: img.blob,
          });
        }
      }
    }

    // Build InboundContext for OpenClaw auto-reply
    const inboundContext = {
      body: fullBody,
      attachments: attachments.length > 0 ? attachments : undefined,
      origin: {
        channel: "aegis",
        location: session.channel,
        chatId: sessionKey,
        senderId: payload.user?.id || "unknown",
        senderName,
        senderUsername,
        timestamp,
      },
    };

    // Send typing indicator
    await sendPresence(session, "thinking");

    // Dispatch to OpenClaw auto-reply system
    try {
      const reply = await this.api.getReplyFromConfig(inboundContext);

      if (reply && reply.text) {
        await sendDone(session, reply.text);
      }
    } catch (err) {
      console.error("[aegis-channel] auto-reply error:", err);
      await sendDone(session, `Error: ${err}`);
    }
  }

  /** Handle outbound message from OpenClaw agent → tether egress. */
  async handleOutbound(message: any): Promise<void> {
    const chatId = message.chatId as string;
    if (!chatId) return;

    // Look up the original tether session
    const session = this.activeSessions.get(chatId);
    if (!session) {
      console.error(
        `[aegis-channel] no tether session for chatId: ${chatId}`
      );
      return;
    }

    const text = message.text || "";

    // Check for image attachments in the outbound message
    let images: ImageRef[] | undefined;
    if (message.attachments && message.attachments.length > 0) {
      images = [];
      for (const att of message.attachments) {
        if (att.type === "image" && att.data) {
          const key = await writeBlob(Buffer.from(att.data), att.mimeType || "image/png");
          images.push({
            media_type: att.mimeType || "image/png",
            blob: key,
            size: att.data.length,
          });
        }
      }
      if (images.length === 0) images = undefined;
    }

    await sendDone(session, text, images);
  }
}

/** Read the full request body as a string. */
function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf-8")));
    req.on("error", reject);
  });
}
