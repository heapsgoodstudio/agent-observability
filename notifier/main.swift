// Agent Observability notifier: posts alert banners that open the dashboard when clicked.
//
// Three modes, chosen by argument count:
//   --request              request notification authorization (install.sh only)
//   <title> <body> <url>   post a banner carrying url; never prompts
//   (no arguments)         relaunched by a click: open the url from the payload
//
// Post mode never prompts on purpose. The usage-alerts launchd agent runs every
// 60s; a process blocked on an unanswered permission dialog would accumulate one
// copy per minute. It exits non-zero instead, and usage-alert.sh falls back to
// osascript so an alert is never lost, only its click target.
//
// Stateless: the repo, the port and the profile are none of its business. Every
// value arrives as an argument or in the notification's userInfo.
import Cocoa
import UserNotifications

let center = UNUserNotificationCenter.current()

func die(_ msg: String, _ code: Int32 = 1) -> Never {
    FileHandle.standardError.write("notifier: \(msg)\n".data(using: .utf8)!)
    exit(code)
}

// Every mode is watchdogged: this process must never outlive its one job.
func watchdog(_ seconds: Double, _ msg: String, _ code: Int32) {
    DispatchQueue.main.asyncAfter(deadline: .now() + seconds) { die(msg, code) }
}

final class Notifier: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    let args = Array(CommandLine.arguments.dropFirst())

    func applicationDidFinishLaunching(_ note: Notification) {
        // Must be set before launch completes, or a click response is dropped.
        center.delegate = self
        switch args.count {
        case 0: awaitClick()
        case 1 where args[0] == "--request": request()
        case 3: post(title: args[0], body: args[1], url: args[2])
        default: die("usage: --request | <title> <body> <url> | (no args: click handler)", 2)
        }
    }

    // install.sh calls this once, interactively, so a prompt is appropriate.
    func request() {
        watchdog(120, "authorization prompt was not answered", 1)
        center.requestAuthorization(options: [.alert, .sound]) { granted, err in
            if let err { die("authorization failed: \(err.localizedDescription)") }
            guard granted else { die("authorization declined", 1) }
            exit(0)
        }
    }

    func post(title: String, body: String, url: String) {
        watchdog(10, "timed out posting", 1)
        center.getNotificationSettings { settings in
            let status = settings.authorizationStatus
            guard status == .authorized || status == .provisional else {
                die("not authorized (status \(status.rawValue)); caller should fall back")
            }
            let content = UNMutableNotificationContent()
            content.title = title
            content.body = body
            content.sound = UNNotificationSound(named: UNNotificationSoundName("Sosumi.aiff"))
            content.userInfo = ["url": url]
            let request = UNNotificationRequest(identifier: UUID().uuidString,
                                                content: content, trigger: nil)
            center.add(request) { err in
                if let err { die("post failed: \(err.localizedDescription)") }
                exit(0)
            }
        }
    }

    // No arguments means macOS relaunched us to deliver a click. If no response
    // arrives, someone opened the app from Finder or Launchpad: just leave.
    func awaitClick() {
        watchdog(15, "no click response; launched by hand?", 3)
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                didReceive response: UNNotificationResponse,
                                withCompletionHandler done: @escaping () -> Void) {
        defer { done() }
        guard let raw = response.notification.request.content.userInfo["url"] as? String,
              let url = URL(string: raw),
              let scheme = url.scheme?.lowercased(),
              scheme == "http" || scheme == "https" else {
            die("notification carried no usable http(s) url", 4)
        }
        NSWorkspace.shared.open(url)
        // Let the open request reach the workspace before the process goes away.
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) { exit(0) }
    }
}

let app = NSApplication.shared
let notifier = Notifier()
app.delegate = notifier
app.setActivationPolicy(.accessory)
app.run()
