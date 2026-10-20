/**
 * Breakroom coordination plugin for OpenCode (2.x and 1.x).
 *
 * Provides:
 * 1. Automatic MCP server configuration (`breakroom mcp`).
 * 2. Stop gate on `session.idle`: parks idle sessions while waiting for peer agents
 *    or blueprint reviews, and automatically wakes the session up when mail arrives.
 * 3. User prompt recording on chat submission to enable roadmap approval from chat.
 *
 * No tool.execute.after mail check: its output never reaches the model, and
 * `breakroom check` marks what it prints as read, so mail would be lost.
 */

// The user's PATH first, so hooks run the same breakroom as the MCP server;
// common install folders after it for GUI launches with a bare PATH.
const defaultPath = [
  process.env.PATH || "",
  `${process.env.HOME || ""}/.local/bin`,
  "/opt/homebrew/bin",
  "/usr/local/bin",
  "/usr/bin",
  "/bin",
].filter(Boolean).join(":")

const runCommand = async (dir: string, args: string[], stdinPayload?: string): Promise<string> => {
  const env = {
    ...process.env,
    PATH: defaultPath,
  }

  try {
    if (typeof Bun !== "undefined" && Bun.spawn) {
      const proc = Bun.spawn(args, {
        stdin: stdinPayload ? new Response(stdinPayload) : undefined,
        stdout: "pipe",
        stderr: "ignore",
        cwd: dir,
        env,
      })
      const out = await new Response(proc.stdout).text()
      await proc.exited
      return out
    }

    // Node.js fallback
    try {
      const { spawn } = await import("node:child_process")
      return await new Promise<string>((resolve) => {
        const child = spawn(args[0], args.slice(1), {
          cwd: dir,
          env,
          stdio: ["pipe", "pipe", "ignore"],
        })
        let output = ""
        child.stdout?.on("data", (chunk: any) => {
          output += chunk.toString()
        })
        child.on("close", () => resolve(output))
        child.on("error", () => resolve(""))
        if (stdinPayload) {
          child.stdin?.write(stdinPayload)
          child.stdin?.end()
        } else {
          child.stdin?.end()
        }
      })
    } catch {
      // Import or execution failed
    }
  } catch {
    // Fail-open
  }
  return ""
}

// Records each human prompt once. Several events report the same message:
// a message id seen before is skipped, and so is the same text within a few
// seconds (sources without an id). Typing the same reply later still counts.
const createPromptRecorder = (dir: string, stopHookActive: Set<string>) => {
  const seenIDs = new Set<string>()
  let lastText = ""
  let lastAt = 0
  return async (promptText: string, messageID?: string) => {
    const text = promptText.trim()
    if (!text || (messageID && seenIDs.has(messageID))) return
    if (messageID) seenIDs.add(messageID)
    const now = Date.now()
    if (text === lastText && now - lastAt < 5000) return
    lastText = text
    lastAt = now
    stopHookActive.clear()

    try {
      const payload = JSON.stringify({
        cwd: dir,
        prompt: text,
      })
      await runCommand(dir, ["breakroom", "gate", "--event", "prompt", "--host", "opencode"], payload)
    } catch {
      // Fail-open
    }
  }
}

const createIdleHandler = (
  dir: string,
  stopHookActive: Set<string>,
  checkingGate: Set<string>,
  wakeSession: (sessionID: string, reason: string) => void
) => {
  return async (sessionID: string) => {
    if (!sessionID || checkingGate.has(sessionID)) return
    checkingGate.add(sessionID)

    try {
      const payload = JSON.stringify({
        cwd: dir,
        stop_hook_active: stopHookActive.has(sessionID),
      })

      const stdout = await runCommand(
        dir,
        ["breakroom", "gate", "--event", "stop", "--host", "opencode"],
        payload
      )

      if (stdout && stdout.trim()) {
        try {
          const res = JSON.parse(stdout)
          if (res.decision === "block" && res.reason) {
            stopHookActive.add(sessionID)
            // Don't await the woken turn: its own session.idle must find
            // the gate free, or the next hold is skipped.
            wakeSession(sessionID, res.reason)
            return
          }
        } catch {
          // Non-JSON stdout or parse error
        }
      }

      stopHookActive.delete(sessionID)
    } catch {
      stopHookActive.delete(sessionID)
    } finally {
      checkingGate.delete(sessionID)
    }
  }
}

export const BreakroomPlugin = {
  id: "breakroom",

  /**
   * OpenCode v2 setup hook.
   */
  async setup(ctx: any) {
    const dir = ctx.location?.directory || ctx.directory || ctx.worktree || "."
    const stopHookActive = new Set<string>()
    const checkingGate = new Set<string>()

    const recordPrompt = createPromptRecorder(dir, stopHookActive)

    const wakeSession = (sessionID: string, reason: string) => {
      try {
        const sessionClient = ctx.client?.session ?? ctx.session
        const send = sessionClient?.promptAsync ?? sessionClient?.prompt
        if (typeof send === "function") {
          // Try standard OpenAPI structure: { path: { id }, body: { parts } }
          const res = send.call(sessionClient, {
            path: { id: sessionID },
            body: {
              parts: [{ type: "text", text: reason }],
            },
          })
          if (res && typeof res.catch === "function") {
            res.catch(() => {
              // Fallback to flat structure: { sessionID, text }
              try {
                send.call(sessionClient, { sessionID, text: reason })?.catch?.(() => {})
              } catch {}
            })
          }
          return
        }

        if (typeof ctx.session?.prompt === "function") {
          ctx.session.prompt({ sessionID, text: reason })?.catch?.(() => {})
        }
      } catch {
        // Fail-open
      }
    }

    const handleIdle = createIdleHandler(dir, stopHookActive, checkingGate, wakeSession)

    // 1. Dynamic MCP server configuration via transform
    if (ctx.mcp?.transform) {
      await ctx.mcp.transform((editor: any) => {
        editor.set("breakroom", {
          type: "local",
          command: ["breakroom", "mcp"],
          enabled: true,
        })
      })
    }

    // 2. Chat prompt recording before model admission
    if (ctx.session?.hook) {
      await ctx.session.hook("prompt", async (event: any) => {
        const text = event.prompt?.text || ""
        if (text) {
          await recordPrompt(text, event.prompt?.id || event.messageID)
        }
      })
    }

    // 3. Stop gate on session.idle via event subscription
    const controller = new AbortController()

    if (ctx.event?.subscribe) {
      void (async () => {
        try {
          for await (const event of ctx.event.subscribe({ signal: controller.signal })) {
            if (!event) continue

            // Fallback user prompt capture from event bus
            if (event.type === "message.updated") {
              const msg = event.properties?.info || event.properties?.message
              const role = msg?.role || msg?.info?.role
              if (role === "user") {
                const text =
                  msg.parts?.map((p: any) => p.text || "").filter(Boolean).join("\n") ||
                  msg.content ||
                  ""
                if (text) {
                  await recordPrompt(text, msg.id || msg.info?.id)
                }
              }
              continue
            }

            const isIdle =
              event.type === "session.idle" ||
              (event.type === "session.status" && event.properties?.status?.type === "idle")

            if (isIdle) {
              const sessionID =
                event.properties?.sessionID ||
                event.properties?.id ||
                event.properties?.session?.id ||
                event.sessionID ||
                event.id

              if (sessionID) {
                // Not awaited: a hold lasts minutes and would stall every
                // other event (prompts, other sessions) behind it.
                void handleIdle(sessionID)
              }
            }
          }
        } catch {
          // Stream aborted on unload or fail-open
        }
      })()
    }

    return () => {
      controller.abort()
    }
  },

  /**
   * OpenCode 1.x server hook (1.18.29 and later).
   */
  async server(ctx: any) {
    const { client, directory, worktree } = ctx
    const dir = directory || worktree || "."

    const stopHookActive = new Set<string>()
    const checkingGate = new Set<string>()

    const recordPrompt = createPromptRecorder(dir, stopHookActive)

    const wakeSession = (sessionID: string, reason: string) => {
      try {
        const send = client?.session?.promptAsync ?? client?.session?.prompt
        if (send) {
          send
            .call(client.session, {
              path: { id: sessionID },
              body: {
                parts: [{ type: "text", text: reason }],
              },
            })
            ?.catch?.(() => {})
        }
      } catch {
        // Fail-open
      }
    }

    const handleIdle = createIdleHandler(dir, stopHookActive, checkingGate, wakeSession)

    return {
      config: async (config: any) => {
        if (!config) return
        config.mcp = config.mcp || {}
        if (!config.mcp.breakroom) {
          config.mcp.breakroom = {
            type: "local",
            command: ["breakroom", "mcp"],
            enabled: true,
          }
        }
      },

      "experimental.chat.messages.transform": async (_input: any, output: any) => {
        try {
          if (output?.messages && Array.isArray(output.messages)) {
            const lastMsg = output.messages[output.messages.length - 1]
            const role = lastMsg?.info?.role || lastMsg?.role
            if (role === "user") {
              const text =
                lastMsg.parts?.map((p: any) => p.text || "").filter(Boolean).join("\n") ||
                lastMsg.content ||
                ""
              if (text) {
                await recordPrompt(text, lastMsg.info?.id || lastMsg.id)
              }
            }
          }
        } catch {
          // Fail-open
        }
      },

      event: async ({ event }: { event: any }) => {
        if (!event) return

        if (event.type === "message.updated") {
          const msg = event.properties?.info || event.properties?.message
          const role = msg?.role || msg?.info?.role
          if (role === "user") {
            const text =
              msg.parts?.map((p: any) => p.text || "").filter(Boolean).join("\n") ||
              msg.content ||
              ""
            if (text) {
              await recordPrompt(text, msg.id || msg.info?.id)
            }
          }
          return
        }

        const isIdle =
          event.type === "session.idle" ||
          (event.type === "session.status" && event.properties?.status?.type === "idle")

        if (isIdle) {
          const sessionID =
            event.properties?.sessionID ||
            event.properties?.id ||
            event.properties?.session?.id ||
            event.sessionID ||
            event.id

          if (sessionID) {
            void handleIdle(sessionID) // see setup(): holds must not block events
          }
        }
      },
    }
  },
}

export default BreakroomPlugin
