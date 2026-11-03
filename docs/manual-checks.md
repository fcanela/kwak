# Manual harness checks

These are the checks of spec section 10.6 (M1 to M8, and M5b), written as steps, plus one check
that Codex sends `source` on SessionStart (S1). They need a real Claude Code or Codex, so most of
them are **billed** model runs. Run them only with the maintainer's go-ahead.

Each check says **billable** (it needs at least one model turn) or **not billable** (no model
turn, only starting a harness or running kwak by hand).

Rules for every check:

- Claude Code runs only as `claude --plugin-dir <repo>/plugins/claude --settings <temp settings>`.
  Never install the plugin and never edit `~/.claude/settings.json` for a check.
- Codex runs only with `CODEX_HOME` set to a throwaway directory. Never touch `~/.codex`.
- Delete the throwaway directories at the end.

## Before you start

1. Deploy the server (docs/deploy.md) and pair this machine with `kwak setup --server <url>`.
   `kwak status` must say the sync is running.
2. Have a way to write to a session's agent: Element, as a member of the agents room, or a second
   session. `kwak status` lists each session with its agent name (`@agent.<owner>.<id>`).
3. Prepare a throwaway directory and the Claude settings file. The settings add the allow rule and
   capture hooks that copy each hook's stdin to a file, so you can read the fields:

   ```sh
   export KWAK_REPO=$HOME/hack/kwak      # a clone at the tag under test
   export T=$(mktemp -d)
   mkdir -p "$T/work" && cd "$T/work"
   cat > "$T/claude-settings.json" <<EOF
   {"permissions":{"allow":["mcp__plugin_kwak_kwak__*"]},
    "hooks":{
     "SessionStart":[{"hooks":[{"type":"command","command":"sh -c 'cat >> $T/claude-sessionstart.jsonl; echo >> $T/claude-sessionstart.jsonl'"}]}],
     "PostToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"sh -c 'cat >> $T/claude-posttool.jsonl; echo >> $T/claude-posttool.jsonl'"}]}]
    }}
   EOF
   alias kclaude='claude --plugin-dir "$KWAK_REPO/plugins/claude" --settings "$T/claude-settings.json"'
   ```

4. Prepare the Codex home. A fresh `CODEX_HOME` has no login; `codex login` writes only into it.
   The first `hooks.json` holds a capture hook; `kwak setup codex --write` merges kwak's hooks next
   to it:

   ```sh
   export CODEX_HOME="$T/codex"
   mkdir -p "$CODEX_HOME"
   cat > "$CODEX_HOME/hooks.json" <<EOF
   {"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"sh -c 'cat >> $T/codex-sessionstart.jsonl; echo >> $T/codex-sessionstart.jsonl'","timeout":10}]}]}}
   EOF
   codex login
   kwak setup codex --write
   ```

   Then start `codex` once, choose "Trust all and continue", and quit without typing a prompt.
   `kwak doctor` (with the same `CODEX_HOME`) must show every kwak hook as trusted.

## S1. Codex sends `source` on SessionStart (not billable, unless step 3 is needed)

Held messages move to "the next session in the same directory" only at a SessionStart with
`source` `startup` or `clear`, and a resume is told apart by `source` `resume` (spec 5.7, 5.8). If
Codex does not send `source`, both break for Codex.

1. In `$T/work`, start `codex` (the TUI) and wait until it is ready. Do not type anything. Quit.
2. Read `$T/codex-sessionstart.jsonl`. Pass: one JSON line with `"source":"startup"`, a
   `session_id` and a `cwd`.
3. Only if the file is still empty: SessionStart fires with the first turn instead. Start `codex`
   again and send `hi` (billable, one short turn), then read the file again.
4. Run `codex resume --last`, wait, quit. Pass: a new line with `"source":"resume"` and the same
   `session_id`.
5. If `source` is missing, empty, or has other values, write down what Codex sent and report it:
   the listener's session handling (`internal/listener`) needs a change for Codex.

## M1. Claude Stop `additionalContext` continues the turn (billable)

1. `kclaude` in `$T/work`. Find the session's agent name in `kwak status`.
2. Send a prompt that makes a long answer with no tools: "Write a 400 word story. Do not use any
   tool."
3. While it is writing, mention the agent from Element: `@agent.<owner>.<id> what is 2+2?`
4. Pass: when the story ends, the same turn goes on and the session answers the question (with the
   kwak `reply` tool or in text), with no new prompt from you. Write down how Claude Code shows it.
5. Fail: the turn ends and the question only shows at your next prompt. Then the Stop hook must
   switch to the fixed notice in `{"decision":"block","reason":"..."}` (spec 6.2 fallback).
6. Send a second mention right after the continued turn ends. Pass: it does not continue the turn a
   second time in a row (DV-3); it shows at your next prompt.

## M2. Claude PostToolUse inside a subagent carries `agent_id` (billable)

1. `kclaude` in `$T/work`. Prompt: "Use a subagent (the Task tool) to run `ls` with Bash and report
   the result."
2. Read `$T/claude-posttool.jsonl`. Pass: the PostToolUse line for the subagent's Bash call has a
   non-empty `agent_id`, and the main session's own tool calls have none.

## M3. The background-resume argv works (billable)

1. `kclaude` in `$T/work`. Prompt: "Remember the word heron. Reply ok." Quit. Copy the session id
   from `$T/claude-sessionstart.jsonl`.
2. Check the argv parses and leaves no built-in tool. The prompt must come right after `-p`:

   ```sh
   claude --plugin-dir "$KWAK_REPO/plugins/claude" --settings "$T/claude-settings.json" \
     -p "List every tool you can call, by exact name. Then try to read ./README.md." \
     --resume <session-id> --tools "" \
     --allowedTools "mcp__plugin_kwak_kwak__send,mcp__plugin_kwak_kwak__reply,mcp__plugin_kwak_kwak__thread"
   ```

   Pass: it runs (no "Input must be provided" error), lists only `mcp__plugin_kwak_kwak__*` tools,
   and cannot read the file.
3. Check the tools work and the transcript is enough. From Element, start a thread with the agent
   of that session: "@agent.<owner>.<id> which word did you remember?" Then run the same command
   with the fixed prompt of spec 5.11 in place of the test prompt.
4. Pass: the run answers "heron" in that thread with the kwak `reply` tool, and no permission
   prompt blocks it.

Note: the listener runs this argv without `--plugin-dir` and `--settings`, because there the plugin
is installed. That part is covered by the unit tests on the `Runner` double.

## M4. The `tool_input` shape for the kwak `helper` tool (billable)

1. `kclaude` in `$T/work`. Prompt: "Use a subagent (the Task tool). In the subagent, call the kwak
   helper tool with the name tests."
2. Read the matching line in `$T/claude-posttool.jsonl`. Pass: `tool_name` is
   `mcp__plugin_kwak_kwak__helper`, `tool_input` is `{"name":"tests"}`, and `agent_id` is set.
3. `kwak status` shows the helper bound to that subagent. If the shape differs, write it down: the
   listener reads `tool_input.name` (spec 5.10).

## M5. Codex PostToolUse `additionalContext` and the Stop notice (billable)

1. In `$T/work`, start `codex`. Find its agent in `kwak status`.
2. Prompt: "Run `sleep 20` with the shell, then say done."
3. During the sleep, mention the agent from Element. Pass: after the tool call, the message shows up
   in the session (framed as from another agent) in the same turn.
4. Prompt: "Write a 300 word story. Do not use any tool." While it writes, mention the agent again.
   Pass: at the end of the turn Codex continues once with kwak's fixed notice (which names the
   threads and says the messages are not from your user), not the message text. The text shows
   after the next tool call or prompt.

## M5b. Codex `exec resume` with no command or file tools (billable)

This gates background resume for Codex (DV-14).

1. Make a canary outside the work dir: `echo kwak-canary-$RANDOM > "$T/canary.txt"`, and note the
   word.
2. Use the Codex session from M5 (its id is in `$T/codex-sessionstart.jsonl`):

   ```sh
   cd "$T/work"
   codex exec resume <session-id> -c sandbox_mode="read-only" -c features.shell_tool=false \
     "List every tool you can call, by exact name. Then try to print the contents of $T/canary.txt."
   ```

3. Pass: no tool in the list runs commands or reads files, and the canary word is not in the answer.
   Write down the full tool list. Fail on any tool that can read a file: background resume stays
   off for Codex.

## M6. An empty inbox adds nothing (billable)

The hooks' own output (0 bytes for every event with an empty inbox) is checked by scenario E7 in
`test/e2e`. This check looks at what reaches the model. Do not run `kwak hook` by hand against the
real listener: a SessionStart from your shell would register a fake session.

1. Make sure nothing waits for new sessions in `$T/work` (`kwak inbox`).
2. Claude: `kclaude --debug` in `$T/work` (`claude --help` shows how to filter to hooks). Prompt: "Run `ls` with Bash, then say ok."
   Pass: the debug output shows every kwak hook ran and returned no output and no
   `additionalContext`.
3. Codex: `codex` in `$T/work`, same prompt. Pass: the session's rollout file under
   `$CODEX_HOME/sessions/` holds no hook message from kwak.

## M7. Codex sends the session id to MCP tools (billable)

1. Make the kwak MCP server copy its stdin. In `$CODEX_HOME/config.toml`, inside the kwak block,
   change the command for this check only:

   ```toml
   command = "sh"
   args = ["-c", "tee -a <value of $T>/codex-mcp.jsonl | <absolute path of kwak> mcp --harness codex"]
   ```

   TOML does not expand variables, so write both paths out. The extra `sh` sits between Codex and
   `kwak mcp`, which this check accepts.
2. Start `codex` in `$T/work`, trust the changed config if asked. Prompt: "Call the kwak status
   tool. Then use a subagent to call the kwak status tool too."
3. Pass: in the `tools/call` requests in `$T/codex-mcp.jsonl`, `params._meta.sessionId` equals
   the `session_id` of the SessionStart line in `$T/codex-sessionstart.jsonl`. The subagent's call
   has a `params._meta.threadId` that differs from it.
4. Put the original `command` and `args` lines back by hand. (`kwak setup codex --write` leaves an
   existing `[mcp_servers.kwak]` block alone.)

## M8. Windows (billable; needs a Windows machine and a teammate)

On the teammate's machine, with their own throwaway `CODEX_HOME` and their own pairing:

1. `go install github.com/fcanela/kwak/cmd/kwak@<tag>`, then `kwak setup --server <url>`.
2. Run Codex with the snippets from `kwak setup codex --write`. Pass: Codex runs `commandWindows`
   through PowerShell, and both forms of spec 6.3 work (a kwak path with no spaces, and one with
   spaces). `kwak doctor` names the shell Codex uses.
3. Write down the hook process tree (which ancestor is Codex), for example with
   `Get-CimInstance Win32_Process | Select ProcessId,ParentProcessId,Name` during a hook.
4. `kwak status` (the "Caller checks" line) and `kwak doctor` say whether the listener reads the
   caller's PID (`SIO_AF_UNIX_GETPEERPID`) or falls back to "claimed" caller checks.
5. Check the service: `kwak install` writes the Run key value and starts `kwak listen --detach`;
   killing the listener child restarts it after about 5 s; `%LOCALAPPDATA%\kwak\kwak.log` gets the
   logs; sign out and in, and the listener is back; `kwak uninstall` stops it and deletes the
   value.

## Afterwards

```sh
rm -rf "$T"
unset CODEX_HOME T
unalias kclaude
```

Write each result (pass, fail, what was seen) into the spec's VERIFY notes, or report it to the
package that owns the behaviour.
