#if os(iOS)
import Foundation
@preconcurrency import ESPProvision

/// All library callbacks enter the main actor. An operation has one result even
/// when cancellation, a late BLE callback and its deadline race each other.
@MainActor
private final class ProvisioningRequest<Value: Sendable> {
    private var continuation: CheckedContinuation<Value, any Error>?
    private var deadline: Task<Void, Never>?

    func run(seconds: Int = 30,
             start: (@escaping @Sendable (Result<Value, ObserverSetupError>) -> Void) -> Void) async throws -> Value {
        try await withTaskCancellationHandler {
            try Task.checkCancellation()
            return try await withCheckedThrowingContinuation { continuation in
                self.continuation = continuation
                deadline = Task { [weak self] in
                    do { try await Task.sleep(for: .seconds(seconds)) } catch { return }
                    self?.finish(.failure(ObserverSetupError.timeout))
                }
                start { [weak self] result in
                    Task { @MainActor in self?.finish(result.mapError { $0 as any Error }) }
                }
            }
        } onCancel: {
            Task { @MainActor in self.finish(.failure(CancellationError())) }
        }
    }

    private func finish(_ result: Result<Value, any Error>) {
        guard let continuation else { return }
        self.continuation = nil
        deadline?.cancel()
        deadline = nil
        continuation.resume(with: result)
    }
}

@MainActor
final class ESPObserverTransport: ObserverProvisioningTransport, @preconcurrency ESPDeviceConnectionDelegate {
    private var device: ESPDevice?
    private var label: ObserverSetupLabel?
    private var generation = 0
    private var onDisconnect: (@Sendable () -> Void)?
    private var hasStartedSearch = false

    func connect(label: ObserverSetupLabel) async throws {
        disconnect()
        self.label = label
        let operation = generation
        defer { if generation == operation { onDisconnect = nil } }
        ESPProvisionManager.shared.enableLogs(false)
        do {
            try await ProvisioningRequest<Void>().run { complete in
                onDisconnect = { complete(.failure(.connection)) }
                hasStartedSearch = true
                // Supply the password through the delegate only after proto-ver
                // confirms Security 2 and our application capability.
                ESPProvisionManager.shared.createESPDevice(deviceName: label.name, transport: .ble,
                                                           security: .secure2, network: .wifi) { [weak self] device, error in
                    Task { @MainActor in
                        guard let self, self.generation == operation else { return }
                        guard error == nil, let device, device.name == label.name else {
                            complete(.failure(.connection)); return
                        }
                        self.device = device
                        device.connect(delegate: self) { [weak self] status in
                            Task { @MainActor in
                                guard let self, self.generation == operation else { return }
                                switch status {
                                case .connected:
                                    complete(self.supports(device) ? .success(()) : .failure(.incompatibleDevice))
                                case .failedToConnect: complete(.failure(.connection))
                                case .disconnected:
                                    self.onDisconnect?()
                                    complete(.failure(.connection))
                                }
                            }
                        }
                    }
                }
            }
        } catch {
            if generation == operation { disconnect() }
            throw error
        }
    }

    func networks() async throws -> [String] {
        let device = try connectedDevice()
        let operation = generation
        defer { if generation == operation { onDisconnect = nil } }
        return try await ProvisioningRequest<[String]>().run { complete in
            onDisconnect = { complete(.failure(.connection)) }
            device.scanWifiList { networks, error in
                guard error == nil, let networks else { complete(.failure(.wifi)); return }
                complete(.success(Array(Set(networks.map(\.ssid))).sorted()))
            }
        }
    }

    func configure(_ data: Data) async throws -> ObserverSetupReply {
        let device = try connectedDevice()
        let operation = generation
        defer { if generation == operation { onDisconnect = nil } }
        return try await ProvisioningRequest<ObserverSetupReply>().run { complete in
            onDisconnect = { complete(.failure(.connection)) }
            device.sendData(path: "nav-config", data: data) { response, error in
                guard error == nil, let response else { complete(.failure(.connection)); return }
                do { complete(.success(try ObserverSetupReply.parse(response))) }
                catch { complete(.failure(.invalidResponse)) }
            }
        }
    }

    func provisionWiFi(ssid: String, password: String) async throws {
        let device = try connectedDevice()
        let operation = generation
        defer { if generation == operation { onDisconnect = nil } }
        try await ProvisioningRequest<Void>().run(seconds: 60) { complete in
            onDisconnect = { complete(.failure(.connection)) }
            device.provision(ssid: ssid, passPhrase: password) { status in
                switch status {
                case .success: complete(.success(()))
                case .configApplied: break
                case .failure(let error):
                    switch error {
                    case .wifiStatusAuthenticationError, .wifiStatusNetworkNotFound,
                         .wifiStatusDisconnected: complete(.failure(.wifi))
                    default: complete(.failure(.connection))
                    }
                }
            }
        }
    }

    func disconnect() {
        let callback = onDisconnect
        onDisconnect = nil
        generation += 1
        ESPProvisionManager.shared.enableLogs(false)
        // ESPProvision's stop method force-unwraps its transport. Closing the
        // setup screen before the first scan must never call it.
        if hasStartedSearch {
            hasStartedSearch = false
            ESPProvisionManager.shared.stopESPDevicesSearch()
        }
        device?.delegate = nil
        device?.disconnect()
        device = nil
        label = nil
        callback?()
    }

    func getProofOfPossesion(forDevice device: ESPDevice, completionHandler: @escaping (String) -> Void) {
        guard supports(device), let label, device === self.device else {
            completionHandler(""); return
        }
        completionHandler(label.pop)
    }

    func getUsername(forDevice device: ESPDevice, completionHandler: @escaping (String?) -> Void) {
        guard supports(device), let label, device === self.device else {
            completionHandler(nil); return
        }
        completionHandler(label.username)
    }

    private func supports(_ device: ESPDevice) -> Bool {
        device.transport == .ble && device.security == .secure2 &&
            ObserverSetupContract.supports(device.versionInfo as? [String: Any])
    }

    private func connectedDevice() throws -> ESPDevice {
        guard let device, supports(device), device.isSessionEstablished() else {
            throw ObserverSetupError.incompatibleDevice
        }
        return device
    }
}
#endif
