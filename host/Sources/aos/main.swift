// aos — AOS host adapter. During kernel preparation this only manages the
// development VM and does not expose the retired agentd syscall surface.
//
// Phase 1 uses the `container` CLI as the VM backend. The backend
// boundary is deliberately contained in ContainerBackend so that a
// direct Containerization-framework backend (snapshot/clone, quotas)
// can replace it without touching the command layer.

import Foundation

// MARK: - Container backend

enum ContainerBackend {
    static func run(_ args: [String], capture: Bool = false) -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/local/bin/container")
        p.arguments = args
        if capture {
            let pipe = Pipe()
            p.standardOutput = pipe
            p.standardError = FileHandle.standardError
            try? p.run()
            p.waitUntilExit()
            let data = pipe.fileHandleForReading.readDataToEndOfFile()
            FileHandle.standardOutput.write(data)
            return p.terminationStatus
        }
        p.standardInput = FileHandle.standardInput
        p.standardOutput = FileHandle.standardOutput
        p.standardError = FileHandle.standardError
        try? p.run()
        p.waitUntilExit()
        return p.terminationStatus
    }
}

// MARK: - Commands

let defaultImage = "aok/subos:dev"

func usage() -> Never {
    print("""
    AOK — Agent-native kernel control

    USAGE:
      aok tui                              # interactive SubOS console
      aok onboard                          # first-run configuration
      aok settings [show|set KEY VALUE]    # inspect or update settings
      aok up    [name] [--image <img>]        # boot a development SubOS microVM
     aok down  [name]                        # remove the SubOS microVM
      aok ls                           # list SubOS instances
      aok exec  <name> -- <cmd...>            # exec inside the SubOS

    The TUI manages the current development VM surface. Agent control is exposed
    through the supervisor control socket when one is configured.
    """)
    exit(2)
}

func args(_ argv: [String]) -> (name: String, flags: [String: String]) {
    guard !argv.isEmpty else { usage() }
    var flags: [String: String] = [:]
    var positional: [String] = []
    var i = 0
    while i < argv.count {
        let a = argv[i]
        if a.hasPrefix("--"), i + 1 < argv.count {
            flags[String(a.dropFirst(2))] = argv[i + 1]
            i += 2
        } else if a.hasPrefix("--") {
            flags[String(a.dropFirst(2))] = "true"
            i += 1
        } else {
            positional.append(a)
            i += 1
        }
    }
    return (positional.first ?? "aos-0", flags)
}

let argv = Array(CommandLine.arguments.dropFirst())
guard let cmd = argv.first else { usage() }
let rest = Array(argv.dropFirst())

switch cmd {
case "tui":
    exit(Interactive.tui())

case "onboard":
    exit(Interactive.onboard())

case "settings":
    exit(Interactive.settings(rest))

case "up":
    let (name, flags) = args(rest)
    let image = flags["image"] ?? defaultImage
    print("aos: booting development SubOS \(name) (\(image))")
    let status = ContainerBackend.run([
        "run", "-d", "--name", name,
        image,
    ])
    exit(status)

case "down":
    let (name, _) = args(rest)
    exit(ContainerBackend.run(["rm", "-f", name]))

case "ls":
    exit(ContainerBackend.run(["ls", "--all"]))

case "exec":
    guard rest.count >= 3, rest[1] == "--" else { usage() }
    exit(ContainerBackend.run(["exec", rest[0]] + Array(rest.dropFirst(2))))

default:
    usage()
}
