/**
 * ai-flow extension for pi.
 *
 * Registers `flow_finish` (how a flow step reports its outcome) and one tool
 * per MCP tool granted to this node. MCP calls go to the ai-flow control plane,
 * which holds the real credentials and re-checks the grant.
 *
 * Inert unless AI_FLOW_BUNDLE is set, so it is safe to load anywhere.
 */
import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { Type } from "typebox";
import { readFileSync, writeFileSync } from "node:fs";

interface MCPTool {
	server: string;
	name: string;
	description: string;
	input_schema: Record<string, unknown>;
}

interface Bundle {
	node: string;
	outcomes: string[];
	result_schema: { properties?: { outputs?: Record<string, unknown> } };
	mcp?: MCPTool[];
	pod_url: string;
}

function toolName(server: string, tool: string): string {
	const clean = (s: string) => s.replace(/[^A-Za-z0-9_]/g, "_");
	return `${clean(server)}__${clean(tool)}`;
}

function textOf(value: unknown): string {
	return typeof value === "string" ? value : JSON.stringify(value, null, 2);
}

export default function (pi: ExtensionAPI) {
	const bundlePath = process.env.AI_FLOW_BUNDLE;
	const resultPath = process.env.AI_FLOW_RESULT;
	const grant = process.env.AI_FLOW_GRANT ?? "";
	if (!bundlePath || !resultPath) return;
	const bundle = JSON.parse(readFileSync(bundlePath, "utf8")) as Bundle;
	const outcomes = bundle.outcomes.filter((o) => o !== "limit" && o !== "timeout");
	const outputsSchema = bundle.result_schema?.properties?.outputs ?? { type: "object", properties: {} };
	let finished = false;

	pi.registerTool({
		name: "flow_finish",
		label: "Finish step",
		description:
			"Finish this flow step. Report the outcome, a one or two sentence summary, and the step's outputs. " +
			"Call it exactly once, as your final action.",
		promptSnippet: "Finish the flow step: outcome, summary, outputs",
		promptGuidelines: [
			`Call flow_finish exactly once when the step is done. Valid outcomes: ${outcomes.join(", ")}.`,
			"After flow_finish succeeds, stop: do not call other tools or write more text.",
		],
		parameters: Type.Object({
			outcome: Type.Unsafe<string>({ type: "string", enum: outcomes, description: `One of: ${outcomes.join(", ")}` }),
			summary: Type.String({ description: "One or two sentences on what you did and why this outcome" }),
			outputs: Type.Unsafe<Record<string, unknown>>(outputsSchema as object),
		}),
		async execute(_id, params) {
			let outputs: unknown = params.outputs ?? {};
			if (typeof outputs === "string") {
				try {
					outputs = JSON.parse(outputs);
				} catch {
					throw new Error("outputs must be a JSON object, not a string");
				}
			}
			if (!outcomes.includes(params.outcome)) {
				throw new Error(`outcome must be one of: ${outcomes.join(", ")}`);
			}
			if (finished) {
				return { content: [{ type: "text", text: "Already finished. Stop now." }], details: {}, terminate: true };
			}
			finished = true;
			writeFileSync(resultPath, JSON.stringify({ outcome: params.outcome, summary: params.summary, outputs }));
			return {
				content: [{ type: "text", text: `Recorded outcome "${params.outcome}". The step is complete. Stop now.` }],
				details: { outcome: params.outcome },
				terminate: true,
			};
		},
	});

	for (const tool of bundle.mcp ?? []) {
		pi.registerTool({
			name: toolName(tool.server, tool.name),
			label: `${tool.server}: ${tool.name}`,
			description: tool.description || `${tool.name} (${tool.server})`,
			promptSnippet: `${tool.server} ${tool.name}: ${(tool.description || "").split("\n")[0]}`,
			parameters: Type.Unsafe<Record<string, unknown>>(
				(tool.input_schema && Object.keys(tool.input_schema).length ? tool.input_schema : { type: "object", properties: {} }) as object,
			),
			async execute(_id, params, signal) {
				const res = await fetch(`${bundle.pod_url}/v1/mcp/call`, {
					method: "POST",
					headers: { "Content-Type": "application/json", Authorization: `Bearer ${grant}` },
					body: JSON.stringify({ server: tool.server, tool: tool.name, arguments: params ?? {} }),
					signal,
				});
				const body = await res.text();
				if (!res.ok) throw new Error(`MCP call failed (${res.status}): ${body}`);
				const parsed = JSON.parse(body) as { content: Array<Record<string, unknown>>; is_error: boolean };
				const text = parsed.content
					.map((c) => (c.type === "text" ? String(c.text ?? "") : textOf(c)))
					.join("\n");
				if (parsed.is_error) throw new Error(text || "MCP tool returned an error");
				return { content: [{ type: "text", text }], details: {} };
			},
		});
	}
}
