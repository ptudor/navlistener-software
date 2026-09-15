import Foundation
import Observation

@MainActor
protocol ObserverProvisioningTransport: AnyObject {
    func connect(label: ObserverSetupLabel) async throws
    func networks() async throws -> [String]
    func configure(_ data: Data) async throws -> ObserverSetupReply
    func configureTunnel(_ data: Data) async throws -> ObserverSetupReply
    func provisionWiFi(ssid: String, password: String) async throws
    func disconnect()
    // True only once connected to a device that advertised the nav-tunnel-v1 capability.
    var supportsTunnel: Bool { get }
}


/// Separate Keychain service and device-only accessibility; no Wi-Fi password
/// or collector enrollment token is retained by the app.
actor ObserverSetupCredentials {
    private let keychain = KeychainConnectionStore(service: "net.intsat.station.observer-setup.v1")
    func password(for name: String) async throws -> String? { try await keychain.token(forServer: name) }
    func remember(_ label: ObserverSetupLabel) async throws {
        try await keychain.saveToken(label.pop, forServer: label.name)
    }
    func forget(_ name: String) async throws { try await keychain.deleteToken(forServer: name) }
}

@MainActor @Observable
final class ObserverProvisioner {
    enum Stage { case idle, connecting, ready, configuring, awaitingCollector, confirmed }
    private(set) var stage = Stage.idle
    private(set) var networks: [String] = []
    private(set) var message: String?
    private(set) var stationID: String?
    private(set) var supportsTunnel = false
    private let transport: any ObserverProvisioningTransport
    private var generation = 0

    init(transport: any ObserverProvisioningTransport) { self.transport = transport }

    var busy: Bool { stage == .connecting || stage == .configuring }

    func connect(_ label: ObserverSetupLabel) async {
        cancel()
        let operation = generation
        stage = .connecting
        do {
            try label.validate()
            try await transport.connect(label: label)
            try requireCurrent(operation)
            // A failed scan still allows a hidden network to be entered.
            let scanned = (try? await transport.networks()) ?? []
            try requireCurrent(operation)
            networks = scanned
            supportsTunnel = transport.supportsTunnel
            stage = .ready
        } catch {
            guard generation == operation, !Task.isCancelled else { return }
            transport.disconnect()
            stage = .idle
            message = error.localizedDescription
        }
    }

    func provision(_ config: ObserverSetupConfig, ssid: String, password: String,
                   tunnel: ObserverTunnelConfig? = nil) async {
        guard stage == .ready else { return }
        let operation = generation
        stage = .configuring
        message = nil
        do {
            let data = try config.encoded()
            let tunnelData = try tunnel?.encoded() // validate before sending anything
            if tunnel != nil && !supportsTunnel { throw ObserverSetupError.tunnelUnsupported }
            guard !ssid.isEmpty, ssid.utf8.count <= 32, !ssid.contains("\0"),
                  password.utf8.count <= 63, !password.contains("\0")
            else { throw ObserverSetupError.invalidConfig }
            let reply = try await transport.configure(data)
            try requireCurrent(operation)
            switch reply {
            case .invalid: throw ObserverSetupError.invalidConfig
            case .saveFailed: throw ObserverSetupError.saveFailed
            case .alreadySaved: throw ObserverSetupError.saveFailed
            case .powerCycle: message = ObserverSetupError.powerCycle.localizedDescription
            case .saved: break
            case .waitingForWiFi:
                // The tunnel profile must arrive before Wi-Fi completes the save, so send it
                // between the collector settings and the Wi-Fi credentials.
                if let tunnelData {
                    let tunnelReply = try await transport.configureTunnel(tunnelData)
                    try requireCurrent(operation)
                    switch tunnelReply {
                    case .invalid: throw ObserverSetupError.invalidTunnel
                    case .saveFailed: throw ObserverSetupError.saveFailed
                    case .alreadySaved: throw ObserverSetupError.tunnelTooLate
                    case .waitingForWiFi, .saved, .powerCycle: break
                    }
                }
                do { try await transport.provisionWiFi(ssid: ssid, password: password) }
                catch ObserverSetupError.connection {
                    // Reboot can beat the final BLE response. Only a new boot
                    // observed at the collector can confirm enrollment.
                    message = String(localized: "setup.disconnected_pending")
                }
            }
            try requireCurrent(operation)
            stationID = config.stationID
            stage = .awaitingCollector
            transport.disconnect()
        } catch {
            guard generation == operation, !Task.isCancelled else { return }
            stage = .ready
            message = error.localizedDescription
        }
    }

    func confirm() { if stage == .awaitingCollector { stage = .confirmed } }

    func cancel() {
        generation += 1
        transport.disconnect()
        networks = []
        stationID = nil
        supportsTunnel = false
        stage = .idle
        message = nil
    }

    private func requireCurrent(_ operation: Int) throws {
        guard operation == generation, !Task.isCancelled else { throw CancellationError() }
    }
}

struct ObserverSetupConfirmation {
    let stationID: String
    let previousBoot: String?
    let collectorTime: Date

    func matches(_ payload: ObserversPayload) -> Bool {
        guard let observer = payload.observers?.first(where: { $0.id == stationID }),
              let sample = observer.board?.latest ?? observer.board?.timing,
              let boot = sample.session, !boot.isEmpty, boot != previousBoot,
              let received = WireDate.parse(sample.receivedAt), received >= collectorTime
        else { return false }
        return true
    }
}
