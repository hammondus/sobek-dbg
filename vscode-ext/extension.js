// Minimal glue: VS Code speaks DAP natively but only through a registered
// debug type, so this extension maps type "sobek" onto a plain TCP connection
// to the host application's DAP server. No adapter process, no logic.
const vscode = require("vscode");

exports.activate = function (context) {
	context.subscriptions.push(
		vscode.debug.registerDebugAdapterDescriptorFactory("sobek", {
			createDebugAdapterDescriptor(session) {
				const c = session.configuration;
				return new vscode.DebugAdapterServer(c.port || 4711, c.host || "127.0.0.1");
			},
		})
	);
};

exports.deactivate = function () {};
