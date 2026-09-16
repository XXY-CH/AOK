import Foundation

struct AOKConfig: Codable, Equatable {
    var image = "aok/subos:dev"
    var stateDirectory = "~/.config/aok/state"
    var controlSocket = ""
    var controlTool = "aokctl"
    var manifest = ""
    var engine = "echo"
    var color = true

    private enum CodingKeys: String, CodingKey {
        case image, stateDirectory, controlSocket, controlTool, manifest, engine, color
    }

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        image = try c.decodeIfPresent(String.self, forKey: .image) ?? "aok/subos:dev"
        stateDirectory = try c.decodeIfPresent(String.self, forKey: .stateDirectory) ?? "~/.config/aok/state"
        controlSocket = try c.decodeIfPresent(String.self, forKey: .controlSocket) ?? ""
        controlTool = try c.decodeIfPresent(String.self, forKey: .controlTool) ?? "aokctl"
        manifest = try c.decodeIfPresent(String.self, forKey: .manifest) ?? ""
        engine = try c.decodeIfPresent(String.self, forKey: .engine) ?? "echo"
        color = try c.decodeIfPresent(Bool.self, forKey: .color) ?? true
    }
}

enum ConfigStore {
    static var directory: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".config", isDirectory: true)
            .appendingPathComponent("aok", isDirectory: true)
    }
    static var file: URL { directory.appendingPathComponent("config.json") }

    static func load() -> AOKConfig {
        guard let data = try? Data(contentsOf: file),
              let config = try? JSONDecoder().decode(AOKConfig.self, from: data) else { return AOKConfig() }
        return config
    }

    static func save(_ config: AOKConfig) throws {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let data = try JSONEncoder.pretty.encode(config)
        try data.write(to: file, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
    }
}

private extension JSONEncoder {
    static var pretty: JSONEncoder {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return encoder
    }
}

private struct SupervisorHealth: Decodable {
    let status: String
    let component: String?
}

private struct SupervisorApplication: Decodable {
    let applicationID: String
    let ownerAgent: String
    let state: String
    let wakePolicy: String
    let checkpoint: String?
    let generation: UInt64
    let tokensUsed: UInt64
    let tokenLimit: UInt64
    let failures: UInt32

    enum CodingKeys: String, CodingKey {
        case applicationID = "application_id"
        case ownerAgent = "owner_agent"
        case state
        case wakePolicy = "wake_policy"
        case checkpoint = "checkpoint_ref"
        case generation
        case tokensUsed = "tokens_used"
        case tokenLimit = "token_limit"
        case failures
    }
}

private struct SupervisorMessage: Decodable {
    let messageID: String
    let status: String
    let sequence: UInt64
    let attempts: UInt32

    enum CodingKeys: String, CodingKey {
        case messageID = "message_id"
        case status, sequence, attempts
    }
}

private struct SupervisorTimer: Decodable {
    let timerID: String
    let due: Int64
    let interval: Int64

    enum CodingKeys: String, CodingKey {
        case timerID = "timer_id"
        case due, interval
    }
}

private enum SupervisorClient {
    fileprivate struct Failure: Error, CustomStringConvertible {
        let description: String
    }

    fileprivate static func call<T: Decodable>(_ config: AOKConfig, _ method: String,
                                               params: [String: Any] = [:], as type: T.Type) -> Result<T, Failure> {
        guard !config.controlSocket.isEmpty else {
            return .failure(Failure(description: "control socket 未配置"))
        }
        guard let paramsData = try? JSONSerialization.data(withJSONObject: params),
              let paramsJSON = String(data: paramsData, encoding: .utf8) else {
            return .failure(Failure(description: "control 参数无法编码"))
        }
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        process.arguments = [config.controlTool.isEmpty ? "aokctl" : config.controlTool,
                             "-socket", config.controlSocket, method, paramsJSON]
        let output = Pipe()
        let errors = Pipe()
        process.standardOutput = output
        process.standardError = errors
        do {
            try process.run()
            process.waitUntilExit()
        } catch {
            return .failure(Failure(description: "无法启动 \(config.controlTool): \(error.localizedDescription)"))
        }
        let stdout = output.fileHandleForReading.readDataToEndOfFile()
        let stderr = errors.fileHandleForReading.readDataToEndOfFile()
        guard process.terminationStatus == 0 else {
            let message = String(data: stderr, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines)
            return .failure(Failure(description: message?.isEmpty == false ? message! : "control 请求失败（\(process.terminationStatus)）"))
        }
        do {
            return .success(try JSONDecoder().decode(type, from: stdout))
        } catch {
            return .failure(Failure(description: "control 返回无法解析: \(error.localizedDescription)"))
        }
    }

    static func callVoid(_ config: AOKConfig, _ method: String, params: [String: Any] = [:]) -> String? {
        switch call(config, method, params: params, as: EmptyResult.self) {
        case .success: return nil
        case .failure(let error): return error.description
        }
    }

    private struct EmptyResult: Decodable {}
}

enum Interactive {
    static func prompt(_ label: String, default value: String) -> String {
        print("\(label) [\(value)]: ", terminator: "")
        let input = readLine()?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        return input.isEmpty ? value : input
    }

    static func onboard() -> Int32 {
        var config = ConfigStore.load()
        print("AOK 首次配置\n")
        print("这些设置保存在 \(ConfigStore.file.path)。")
        config.image = prompt("SubOS 镜像", default: config.image)
        config.stateDirectory = prompt("状态目录", default: config.stateDirectory)
        config.engine = prompt("默认引擎 (echo/llama)", default: config.engine)
        config.controlSocket = prompt("Control socket（可留空）", default: config.controlSocket)
        config.controlTool = prompt("aokctl 路径或命令", default: config.controlTool)
        config.manifest = prompt("能力 manifest（可留空）", default: config.manifest)
        do {
            try ConfigStore.save(config)
            print("\n配置已保存。")
            onboardingChecks(config)
            print("运行 `aok tui` 查看运行状态。")
            return 0
        } catch {
            fputs("保存配置失败: \(error)\n", stderr)
            return 1
        }
    }

    static func settings(_ args: [String]) -> Int32 {
        var config = ConfigStore.load()
        if args.first == "show" || args.isEmpty {
            printConfig(config)
            return 0
        }
        guard args.first == "set", args.count == 3 else {
            print("用法: aok settings [show | set image|state-directory|control-socket|control-tool|manifest|engine|color VALUE]")
            return 2
        }
        switch args[1] {
        case "image": config.image = args[2]
        case "state-directory": config.stateDirectory = args[2]
        case "control-socket": config.controlSocket = args[2]
        case "control-tool": config.controlTool = args[2]
        case "manifest": config.manifest = args[2]
        case "engine":
            guard args[2] == "echo" || args[2] == "llama" else { print("engine 必须是 echo 或 llama"); return 2 }
            config.engine = args[2]
        case "color":
            guard args[2] == "true" || args[2] == "false" else { print("color 必须是 true 或 false"); return 2 }
            config.color = args[2] == "true"
        default: print("未知设置: \(args[1])"); return 2
        }
        do { try ConfigStore.save(config); print("已更新 \(args[1])") ; return 0 }
        catch { fputs("保存配置失败: \(error)\n", stderr); return 1 }
    }

    static func tui() -> Int32 {
        var config = ConfigStore.load()
        while true {
            print("\u{001B}[2J\u{001B}[H", terminator: "")
            print("AOK  SubOS 控制台")
            print("────────────────────────────────────────")
            print("镜像       \(config.image)")
            print("引擎       \(config.engine)")
            print("配置       \(ConfigStore.file.path)")
            if config.controlSocket.isEmpty {
                print("Supervisor  未配置")
            } else {
                switch SupervisorClient.call(config, "health", as: SupervisorHealth.self) {
                case .success(let health): print("Supervisor  \(health.status)\(health.component.map { " / \($0)" } ?? "")")
                case .failure(let error): print("Supervisor  不可用（\(error)）")
                }
            }
            print("")
            print("[1] 查看 SubOS 实例")
            print("[2] Supervisor Overview")
            print("[3] Applications")
            print("[4] 启动实例")
            print("[5] 设置")
            print("[q] 退出")
            print("\n选择: ", terminator: "")
            guard let choice = readLine()?.lowercased() else { return 0 }
            switch choice {
            case "1":
                print("")
                _ = ContainerBackend.run(["ls", "--all"], capture: false)
                pause()
            case "2":
                supervisorOverview(config)
                pause()
            case "3":
                applications(config)
            case "4":
                let name = prompt("实例名称", default: "aok-0")
                print("启动 \(name)…")
                _ = ContainerBackend.run(["run", "-d", "--name", name, config.image])
                pause()
            case "5":
                config = settingsInteractive(config)
                pause()
            case "q", "quit", "exit": return 0
            default: print("无效选择"); pause()
            }
        }
    }

    private static func printConfig(_ config: AOKConfig) {
        print("image            \(config.image)")
        print("state-directory  \(config.stateDirectory)")
        print("control-socket   \(config.controlSocket.isEmpty ? "(未设置)" : config.controlSocket)")
        print("control-tool     \(config.controlTool)")
        print("manifest         \(config.manifest.isEmpty ? "(未设置)" : config.manifest)")
        print("engine           \(config.engine)")
        print("color            \(config.color)")
    }

    private static func settingsInteractive(_ current: AOKConfig) -> AOKConfig {
        var config = current
        print("\n设置：")
        print("[1] 镜像  [2] 状态目录  [3] 引擎  [4] Control socket  [5] aokctl  [6] manifest  [7] 颜色")
        print("选择（Enter 返回）: ", terminator: "")
        guard let choice = readLine(), !choice.isEmpty else { return config }
        switch choice {
        case "1": config.image = prompt("SubOS 镜像", default: config.image)
        case "2": config.stateDirectory = prompt("状态目录", default: config.stateDirectory)
        case "3":
            let value = prompt("默认引擎 (echo/llama)", default: config.engine)
            if value == "echo" || value == "llama" { config.engine = value } else { print("引擎必须是 echo 或 llama") }
        case "4": config.controlSocket = prompt("Control socket", default: config.controlSocket)
        case "5": config.controlTool = prompt("aokctl 路径或命令", default: config.controlTool)
        case "6": config.manifest = prompt("能力 manifest", default: config.manifest)
        case "7": config.color = prompt("颜色 (true/false)", default: String(config.color)) == "true"
        default: print("无效选择")
        }
        do { try ConfigStore.save(config); print("设置已保存") }
        catch { print("保存配置失败: \(error)") }
        return config
    }

    private static func pause() {
        print("\n按 Enter 返回…", terminator: "")
        _ = readLine()
    }

    private static func onboardingChecks(_ config: AOKConfig) {
        print("\n配置检查：")
        let container = FileManager.default.isExecutableFile(atPath: "/usr/local/bin/container")
        print("[\(container ? "ok" : "--")] container CLI")
        if config.manifest.isEmpty {
            print("[--] manifest 未配置")
        } else {
            let exists = FileManager.default.fileExists(atPath: config.manifest)
            print("[\(exists ? "ok" : "--")] manifest \(config.manifest)")
        }
        if config.controlSocket.isEmpty {
            print("[--] supervisor control socket 未配置")
        } else {
            switch SupervisorClient.call(config, "health", as: SupervisorHealth.self) {
            case .success: print("[ok] supervisor control socket")
            case .failure(let error): print("[--] supervisor control socket: \(error)")
            }
        }
    }

    private static func supervisorOverview(_ config: AOKConfig) {
        print("\nSupervisor Overview")
        switch SupervisorClient.call(config, "health", as: SupervisorHealth.self) {
        case .success(let health): print("health      \(health.status)")
        case .failure(let error): print("health      \(error)")
        }
        switch SupervisorClient.call(config, "application.list", as: [SupervisorApplication].self) {
        case .success(let apps):
            print("applications \(apps.count)")
            for app in apps { printApplication(app) }
        case .failure(let error): print("applications \(error)")
        }
    }

    private static func applications(_ config: AOKConfig) {
        guard !config.controlSocket.isEmpty else { print("\n请先在设置中配置 control socket"); pause(); return }
        while true {
            print("\nApplications")
            switch SupervisorClient.call(config, "application.list", as: [SupervisorApplication].self) {
            case .success(let apps):
                if apps.isEmpty { print("暂无 Application") }
                for (index, app) in apps.enumerated() { print("[\(index + 1)] ", terminator: ""); printApplication(app) }
                print("[c] 创建  [Enter] 返回")
                print("选择: ", terminator: "")
                let choice = readLine()?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                if choice.isEmpty { return }
                if choice.lowercased() == "c" { createApplication(config); continue }
                guard let index = Int(choice), index > 0, index <= apps.count else { print("无效选择"); continue }
                applicationDetail(config, app: apps[index - 1])
            case .failure(let error): print(error); pause(); return
            }
        }
    }

    private static func printApplication(_ app: SupervisorApplication) {
        let limit = app.tokenLimit == 0 ? "unlimited" : String(app.tokenLimit)
        print("\(app.applicationID.prefix(12))  \(app.state)  owner=\(app.ownerAgent)  tokens=\(app.tokensUsed)/\(limit) failures=\(app.failures)")
    }

    private static func createApplication(_ config: AOKConfig) {
        let owner = prompt("owner agent", default: "aok-user")
        let wake = prompt("wake policy (on_event/manual/on_quiescent)", default: "on_event")
        switch SupervisorClient.call(config, "application.create", params: ["owner_agent": owner, "wake_policy": wake], as: SupervisorApplication.self) {
        case .success(let app): print("已创建 \(app.applicationID)")
        case .failure(let error): print("创建失败: \(error)")
        }
        pause()
    }

    private static func applicationDetail(_ config: AOKConfig, app: SupervisorApplication) {
        print("\nApplication \(app.applicationID)")
        printApplication(app)
        switch SupervisorClient.call(config, "mailbox.list", params: ["application_id": app.applicationID], as: [SupervisorMessage].self) {
        case .success(let messages):
            print("mailbox     \(messages.count) records")
            for message in messages.suffix(8) { print("  \(message.messageID) \(message.status) seq=\(message.sequence) attempts=\(message.attempts)") }
        case .failure(let error): print("mailbox     \(error)")
        }
        switch SupervisorClient.call(config, "event_source.list", params: ["application_id": app.applicationID], as: [SupervisorTimer].self) {
        case .success(let timers):
            print("timers      \(timers.count)")
            for timer in timers { print("  \(timer.timerID) due=\(timer.due) interval=\(timer.interval)ns") }
        case .failure(let error): print("timers      \(error)")
        }
        print("[f] 冻结  [r] 恢复  [b] 设置 token limit  [Enter] 返回")
        print("操作: ", terminator: "")
        switch readLine()?.lowercased() {
        case "f": printResult(SupervisorClient.callVoid(config, "application.freeze", params: ["application_id": app.applicationID]))
        case "r": printResult(SupervisorClient.callVoid(config, "application.resume", params: ["application_id": app.applicationID]))
        case "b":
            let value = prompt("token limit (0=unlimited)", default: String(app.tokenLimit))
            if let limit = UInt64(value) { printResult(SupervisorClient.callVoid(config, "budget.set", params: ["application_id": app.applicationID, "token_limit": limit])) }
            else { print("无效 token limit") }
        default: break
        }
        pause()
    }

    private static func printResult(_ error: String?) {
        print(error.map { "失败: \($0)" } ?? "已完成")
    }
}
