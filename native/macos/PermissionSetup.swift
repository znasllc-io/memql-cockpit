import AppKit

struct PermissionReport {
    let pid: Int
    let version: String
    let executable: String?
    let accessibility: String
    let screenRecording: String
    let requestsSupported: Bool
    let requestPending: Bool
    let checkedAt: Date?

    func fresh(at now: Date = Date()) -> Bool {
        guard let checkedAt else { return false }
        return now.timeIntervalSince(checkedAt) >= -5 && now.timeIntervalSince(checkedAt) <= 15
    }
    func granted(at now: Date = Date()) -> Bool {
        fresh(at: now) && accessibility == "Granted" && screenRecording == "Granted"
    }
}

enum PermissionBridge {
    static func accepts(_ url: URL) -> Bool {
        url.scheme == "memql-cockpit" && url.host == "permissions" &&
        (url.path.isEmpty || url.path == "/") && url.user == nil && url.password == nil &&
        url.port == nil && url.query == nil && url.fragment == nil
    }

    static func serviceMatches(_ output: String, pid: Int, executable: String) -> Bool {
        let lines = output.components(separatedBy: .newlines).map { $0.trimmingCharacters(in: .whitespaces) }
        return lines.contains("pid = \(pid)") && lines.contains("program = \(executable)")
    }
}

struct PermissionRestartProgress {
    private(set) var pid: Int?
    private var deadline: Date?
    mutating func begin(pid: Int, now: Date = Date()) { self.pid = pid; deadline = now.addingTimeInterval(30) }
    mutating func cancel() { pid = nil; deadline = nil }
    mutating func observe(_ report: PermissionReport?, now: Date = Date()) -> String? {
        guard let oldPID = pid else { return nil }
        if let report, report.fresh(at: now), report.pid != oldPID {
            cancel()
            return "Worker restarted. Checking access from the new process."
        }
        if let deadline, now > deadline {
            cancel()
            return "A new worker process has not reported back. Open Cockpit Logs to check the service."
        }
        return nil
    }
    func ready(_ report: PermissionReport?, now: Date = Date()) -> Bool {
        pid == nil && report?.granted(at: now) == true
    }
}

// The window only reflects observations. A successful request or restart is
// never promoted to a grant; the next fresh worker report supplies that fact.
final class PermissionSetupWindow: NSWindowController {
    var request: ((String, Int) -> Void)?
    var restart: ((Int) -> Void)?
    var reveal: (() -> Void)?
    private var report: PermissionReport?
    private var progress = PermissionRestartProgress()
    private var message = ""
    private let summary = NSTextField(wrappingLabelWithString: "Connecting to the background worker…")
    private let identity = NSTextField(wrappingLabelWithString: "")
    private let feedback = NSTextField(wrappingLabelWithString: "")
    private let axStatus = NSTextField(labelWithString: "Unknown")
    private let screenStatus = NSTextField(labelWithString: "Unknown")
    private var axRequest: NSButton!
    private var screenRequest: NSButton!
    private var restartButton: NSButton!

    init() {
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 650, height: 455),
                              styleMask: [.titled, .closable, .miniaturizable], backing: .buffered, defer: false)
        window.title = "MemQL Cockpit — Permissions"
        window.isReleasedWhenClosed = false
        super.init(window: window)
        let heading = NSTextField(labelWithString: "Connect this Mac’s screen and controls")
        heading.font = .systemFont(ofSize: 21, weight: .semibold)
        summary.font = .systemFont(ofSize: 13)
        identity.font = .monospacedSystemFont(ofSize: 11, weight: .regular)
        identity.textColor = .secondaryLabelColor
        identity.isSelectable = true
        feedback.font = .systemFont(ofSize: 12)
        axRequest = NSButton(title: "Request Accessibility", target: self, action: #selector(requestAX))
        screenRequest = NSButton(title: "Request Screen Recording", target: self, action: #selector(requestScreen))
        let ax = permissionRow("Accessibility", detail: "Allows mouse and keyboard control.", status: axStatus,
                               request: axRequest, settings: #selector(openAX))
        let screen = permissionRow("Screen Recording", detail: "Allows the worker to see your screen.", status: screenStatus,
                                   request: screenRequest, settings: #selector(openScreen))
        let reveal = NSButton(title: "Show Worker in Finder", target: self, action: #selector(showWorker))
        restartButton = NSButton(title: "Restart Worker…", target: self, action: #selector(confirmRestart))
        let recovery = NSStackView(views: [reveal, restartButton]); recovery.spacing = 12
        let tip = NSTextField(wrappingLabelWithString: "If Settings shows an enabled entry but access stays denied, use Show Worker in Finder to check the installed file. After changing access in Settings, restart the worker here if needed.")
        tip.font = .systemFont(ofSize: 12); tip.textColor = .secondaryLabelColor
        let stack = NSStackView(views: [heading, summary, ax, screen, recovery, tip, feedback, identity])
        stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = 16
        stack.translatesAutoresizingMaskIntoConstraints = false
        window.contentView!.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: window.contentView!.leadingAnchor, constant: 24),
            stack.trailingAnchor.constraint(equalTo: window.contentView!.trailingAnchor, constant: -24),
            stack.topAnchor.constraint(equalTo: window.contentView!.topAnchor, constant: 24),
            stack.bottomAnchor.constraint(lessThanOrEqualTo: window.contentView!.bottomAnchor, constant: -24)
        ])
        for view in [summary, ax, screen, tip, feedback, identity] {
            view.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        }
        window.center()
        update(nil)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    private func permissionRow(_ title: String, detail: String, status: NSTextField, request: NSButton, settings: Selector) -> NSStackView {
        let name = NSTextField(labelWithString: title); name.font = .systemFont(ofSize: 14, weight: .semibold)
        let description = NSTextField(labelWithString: detail); description.font = .systemFont(ofSize: 12)
        description.textColor = .secondaryLabelColor
        let text = NSStackView(views: [name, description, status]); text.orientation = .vertical; text.alignment = .leading; text.spacing = 3
        let settings = NSButton(title: "Open Settings", target: self, action: settings)
        let spacer = NSView()
        let row = NSStackView(views: [text, spacer, request, settings]); row.spacing = 8
        return row
    }

    func update(_ report: PermissionReport?) {
        self.report = report
        let fresh = report?.fresh() == true
        if let result = progress.observe(report) { message = result }
        let restarting = progress.pid != nil
        let supported = fresh && report?.requestsSupported == true
        let pending = report?.requestPending == true
        axStatus.stringValue = fresh ? report!.accessibility : "Unknown — waiting for worker"
        screenStatus.stringValue = fresh ? report!.screenRecording : "Unknown — waiting for worker"
        axRequest.isEnabled = supported && !pending && !restarting && report?.accessibility != "Granted"
        screenRequest.isEnabled = supported && !pending && !restarting && report?.screenRecording != "Granted"
        restartButton.isEnabled = fresh && report?.executable != nil && !pending && !restarting
        if restarting {
            summary.stringValue = "Waiting for the restarted worker to report its permissions…"
        } else if progress.ready(report) {
            summary.stringValue = "Ready. The running worker confirms both permissions are granted."
        } else if !fresh {
            summary.stringValue = "Waiting for a fresh report from the background worker. Access is not yet verified."
        } else if !supported {
            summary.stringValue = "Guided requests need an updated macOS computer-use worker. Install or update Cockpit, then reopen this window."
        } else {
            summary.stringValue = "Request each permission, then approve it in macOS. This window checks the running worker automatically."
        }
        feedback.stringValue = pending ? "Waiting for the macOS permission dialog…" : message
        identity.stringValue = report.map { "Worker \($0.version) · PID \($0.pid)\n\($0.executable ?? "Executable path unavailable from this worker")" } ?? "Worker unavailable"
    }
    func actionFinished(_ error: String?) {
        message = error ?? "Request sent. Approve access in macOS; access stays unverified until the worker reports it."
        update(report)
    }
    func restartFinished(_ error: String?) {
        if let error { progress.cancel(); message = error }
        update(report)
    }
    @objc private func requestAX() { requestPermission("accessibility") }
    @objc private func requestScreen() { requestPermission("screen_recording") }
    private func requestPermission(_ permission: String) {
        guard let report, report.fresh(), report.requestsSupported else { return }
        axRequest.isEnabled = false; screenRequest.isEnabled = false
        request?(permission, report.pid)
    }
    @objc private func openAX() { NSWorkspace.shared.open(URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility")!) }
    @objc private func openScreen() { NSWorkspace.shared.open(URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture")!) }
    @objc private func showWorker() { reveal?() }
    @objc private func confirmRestart() {
        guard let report, report.fresh() else { return }
        let alert = NSAlert()
        alert.messageText = "Restart the background worker?"
        alert.informativeText = "This interrupts running work and briefly disconnects every server on this Mac. Your enrollments and paused connections are preserved. macOS permissions will be checked again by the new worker process."
        alert.addButton(withTitle: "Cancel"); alert.addButton(withTitle: "Restart Worker")
        guard alert.runModal() == .alertSecondButtonReturn else { return }
        progress.begin(pid: report.pid)
        update(self.report)
        restart?(report.pid)
    }
}
