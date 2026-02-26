/**
 * @aegis/openclaw-channel-aegis — AegisVM tether channel extension for OpenClaw.
 *
 * Bridges the AegisVM tether protocol to OpenClaw's channel system.
 * Runs in-process with the OpenClaw gateway — no separate process, no WebSocket hop.
 *
 * Ingress: HTTP server on :7778 receives tether frames from the harness,
 *          normalizes to OpenClaw MsgContext, dispatches via dispatchInboundMessage.
 * Egress:  ReplyDispatcher deliver callback emits tether frames.
 *
 * This extension imports OpenClaw internals directly (auto-reply, config, routing)
 * — the same pattern used by OpenClaw's own Telegram and WhatsApp channels.
 */

import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import {
  type TetherFrame,
  type UserMessagePayload,
  sendPresence,
  sendDelta,
  sendDone,
  sendReady,
  readBlob,
  writeBlob,
  type ImageRef,
} from "./tether.js";

// OpenClaw internal imports — resolved at runtime (OpenClaw is installed globally in the VM).
// These modules aren't available at build time, so we use createRequire to load them
// dynamically. This follows the same import pattern as OpenClaw's own channels.
import { createRequire } from "node:module";

let oc: {
  dispatchInboundMessageWithDispatcher: any;
  loadConfig: any;
  resolveAgentRoute: any;
  resolveSessionAgentId: any;
  createReplyPrefixOptions: any;
};

function loadOpenClawModules(): void {
  // createRequire bypasses TypeScript's static module resolution
  const req = createRequire(require.resolve("openclaw/package.json"));
  const dispatch = req("openclaw/auto-reply/dispatch.js");
  const config = req("openclaw/config/config.js");
  const routing = req("openclaw/routing/resolve-route.js");
  const agents = req("openclaw/agents/agent-scope.js");
  const replyPrefix = req("openclaw/channels/reply-prefix.js");

  oc = {
    dispatchInboundMessageWithDispatcher: dispatch.dispatchInboundMessageWithDispatcher,
    loadConfig: config.loadConfig,
    resolveAgentRoute: routing.resolveAgentRoute,
    resolveSessionAgentId: agents.resolveSessionAgentId,
    createReplyPrefixOptions: replyPrefix.createReplyPrefixOptions,
  };
}

const LISTEN_PORT = 7778;
const AEGIS_CHANNEL_ID = "aegis";

/**
 * OpenClaw plugin registration entry point.
 * Called by the OpenClaw gateway at startup when this extension is discovered.
 */
export default function register(api: any): void {
  console.log("[aegis-channel] registering AegisVM tether channel");

  const channel = new AegisChannel();

  // Register as a channel plugin (for config discovery and outbound routing)
  api.registerChannel({
    id: AEGIS_CHANNEL_ID,
    meta: {
      id: AEGIS_CHANNEL_ID,
      label: "AegisVM Tether",
      selectionLabel: "AegisVM",
      docsPath: "",
      blurb: "Bridges AegisVM tether protocol to OpenClaw",
    },
    capabilities: {
      chatTypes: ["direct", "group"],
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
      deliveryMode: "direct",
      sendText: async (ctx: any) => {
        await channel.handleOutbound(ctx);
        return { channel: AEGIS_CHANNEL_ID };
      },
    },
  });

  // Start the tether HTTP server
  channel.start();
}

/**
 * AegisChannel implements the bridge between tether and OpenClaw.
 *
 * Message dispatch follows the same pattern as OpenClaw's gateway chat.send:
 * build MsgContext → resolve route → create ReplyDispatcher → dispatchInboundMessage.
 */
class AegisChannel {
  private activeSessions = new Map<string, { channel: string; id: string }>();

  /** Start the HTTP server for tether frame ingress. */
  start(): void {
    // Load OpenClaw modules (available at runtime, not at build time)
    loadOpenClawModules();
    console.log("[aegis-channel] OpenClaw modules loaded");

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
      sendReady().catch(() => {});
    });
  }

  /** Handle an incoming tether frame from the harness. */
  private async handleTetherRecv(
    req: IncomingMessage,
    res: ServerResponse,
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

  /**
   * Process an incoming user message: build MsgContext and dispatch to OpenClaw.
   * Follows the same pattern as gateway/server-methods/chat.ts chat.send handler.
   */
  private async handleUserMessage(frame: TetherFrame): Promise<void> {
    const payload = frame.payload as UserMessagePayload;
    if (!payload) return;

    const session = frame.session;
    const sessionKey = `${session.channel}_${session.id}`;
    const tetherSessionKey = `${session.channel}:${session.id}`;

    // Track the tether session for egress routing
    this.activeSessions.set(tetherSessionKey, session);

    // Send typing indicator
    await sendPresence(session, "thinking");

    const senderName = payload.user?.name || payload.user?.username || "User";
    const senderUsername = payload.user?.username || "";
    const senderId = payload.user?.id || "unknown";
    const messageText = payload.text || "";
    const timestamp = frame.ts || new Date().toISOString();

    // Build envelope header (OpenClaw convention)
    const channelLabel = session.channel.charAt(0).toUpperCase() + session.channel.slice(1);
    let envelope = `[${channelLabel} id:${session.id} ${timestamp}]`;
    envelope += senderUsername
      ? `\nSender: ${senderName} (@${senderUsername})`
      : `\nSender: ${senderName}`;
    const fullBody = envelope + "\n\n" + messageText;

    // Resolve image attachments from blob store
    const mediaPaths: string[] = [];
    const mediaTypes: string[] = [];
    if (payload.images && payload.images.length > 0) {
      for (const img of payload.images) {
        const data = await readBlob(img.blob);
        if (data) {
          // Write to a temp location that OpenClaw can read
          const mediaPath = `/workspace/.aegis/blobs/${img.blob}`;
          mediaPaths.push(mediaPath);
          mediaTypes.push(img.media_type);
        }
      }
    }

    // Load config and resolve route
    const cfg = oc.loadConfig();
    const route = oc.resolveAgentRoute({
      cfg,
      channel: AEGIS_CHANNEL_ID,
      accountId: "default",
      peer: { kind: "direct", id: tetherSessionKey },
    });

    // Build MsgContext — same structure as gateway chat.send and Telegram channel
    const ctx: any = {
      Body: messageText,
      BodyForAgent: fullBody,
      RawBody: messageText,
      CommandBody: messageText,
      BodyForCommands: messageText,
      SessionKey: route.sessionKey,
      AccountId: route.accountId,
      Provider: AEGIS_CHANNEL_ID,
      Surface: AEGIS_CHANNEL_ID,
      OriginatingChannel: AEGIS_CHANNEL_ID,
      OriginatingTo: tetherSessionKey,
      ChatType: "direct",
      SenderId: senderId,
      SenderName: senderName,
      SenderUsername: senderUsername,
      Timestamp: Math.floor(new Date(timestamp).getTime()),
      CommandAuthorized: true,
    };

    // Add media paths if images present
    if (mediaPaths.length > 0) {
      ctx.MediaPaths = mediaPaths;
      ctx.MediaTypes = mediaTypes;
      ctx.MediaPath = mediaPaths[0];
      ctx.MediaType = mediaTypes[0];
    }

    const agentId = oc.resolveSessionAgentId({ sessionKey: route.sessionKey, config: cfg });
    const { onModelSelected, ...prefixOptions } = oc.createReplyPrefixOptions({
      cfg,
      agentId,
      channel: AEGIS_CHANNEL_ID,
    });

    // Collect response text for tether egress
    let responseText = "";

    try {
      await oc.dispatchInboundMessageWithDispatcher({
        ctx,
        cfg,
        dispatcherOptions: {
          ...prefixOptions,
          onError: (err: any) => {
            console.error("[aegis-channel] dispatch error:", err);
          },
          deliver: async (replyPayload: any, info: { kind: string }) => {
            const text = replyPayload.text?.trim() ?? "";
            if (!text) return;

            if (info.kind === "final") {
              responseText += (responseText ? "\n\n" : "") + text;
            } else {
              // Intermediate delivery — send as delta
              await sendDelta(session, text);
            }
          },
        },
        replyOptions: {
          onModelSelected,
        },
      });

      // Send final response
      await sendDone(session, responseText || "No response generated.");
    } catch (err) {
      console.error("[aegis-channel] dispatch error:", err);
      await sendDone(session, `Error: ${err}`);
    }
  }

  /** Handle outbound message from OpenClaw agent → tether egress. */
  async handleOutbound(ctx: any): Promise<void> {
    const to = ctx.to as string;
    if (!to) return;

    const session = this.activeSessions.get(to);
    if (!session) {
      console.error(`[aegis-channel] no tether session for: ${to}`);
      return;
    }

    const text = ctx.text || "";
    await sendDone(session, text);
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
