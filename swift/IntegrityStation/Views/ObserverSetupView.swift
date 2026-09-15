#if os(iOS)
import SwiftUI
import VisionKit
import AVFoundation

struct ObserverSetupView: View {
    @Environment(AppController.self) private var controller
    @Environment(\.dismiss) private var dismiss
    @State private var provisioner = ObserverProvisioner(transport: ESPObserverTransport())
    @State private var name = ""
    @State private var password = ""
    @State private var remember = false
    @State private var ssid = ""
    @State private var wifiPassword = ""
    @State private var host = ""
    @State private var port = "5580"
    @State private var stationID = ""
    @State private var enrollmentToken = ""
    @State private var message: String?
    @State private var scanning = false
    @State private var checking = false
    @State private var operationTask: Task<Void, Never>?
    @State private var confirmation: ObserverSetupConfirmation?
    @State private var confirmationSession: ReadSession?
    private let credentials = ObserverSetupCredentials()
    private let feed = FeedClient()

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Text("setup.introduction")
                    Text("setup.tokens_note").font(.caption).foregroundStyle(.secondary)
                }
                if provisioner.stage == .idle || provisioner.stage == .connecting {
                    labelSection
                }
                if provisioner.stage == .ready || provisioner.stage == .configuring {
                    networkSection
                    collectorSection
                    Section {
                        Button("setup.provision") {
                            operationTask = Task { await provision() }
                        }
                        .disabled(provisioner.busy || checking)
                        Button("setup.reconnect") {
                            operationTask?.cancel()
                            provisioner.cancel()
                            message = nil
                        }
                        .disabled(provisioner.busy || checking)
                    }
                }
                if provisioner.stage == .awaitingCollector {
                    Section("setup.confirm.title") {
                        Text("setup.confirm.pending")
                        Button("setup.confirm.check") { operationTask = Task { await checkCollector() } }
                            .disabled(checking || confirmation == nil)
                        if confirmation == nil { Text("setup.confirm.private_required").font(.caption) }
                    }
                }
                if provisioner.stage == .confirmed {
                    Section {
                        Label("setup.confirm.success", systemImage: "checkmark.circle.fill")
                            .foregroundStyle(StationPalette.ok)
                    }
                }
                if provisioner.busy || checking { ProgressView() }
                if let message = message ?? provisioner.message {
                    Section { Text(message).foregroundStyle(StationPalette.warning) }
                }
                Section("setup.recovery.title") {
                    Text("setup.recovery.instructions").font(.caption)
                    Link("setup.recovery.portal", destination: URL(string: "http://192.168.4.1/")!)
                }
            }
            .autocorrectionDisabled()
            .textInputAutocapitalization(.never)
            .navigationTitle(Text("setup.title"))
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("action.done") { dismiss() }
                }
            }
            .sheet(isPresented: $scanning) {
                NavigationStack {
                    SetupLabelScanner { result in
                        scanning = false
                        switch result {
                        case .success(let json):
                            do {
                                let label = try ObserverSetupLabel.parse(json)
                                name = label.name
                                password = label.pop
                                message = nil
                            } catch { message = error.localizedDescription }
                        case .failure: message = String(localized: "setup.error.camera")
                        }
                    }
                    .navigationTitle(Text("setup.scan"))
                    .toolbar { ToolbarItem(placement: .cancellationAction) { Button("action.done") { scanning = false } } }
                }
            }
        }
        .onAppear { if host.isEmpty { host = controller.serverURL?.host ?? "" } }
        .onDisappear {
            operationTask?.cancel()
            provisioner.cancel()
            password = ""
            wifiPassword = ""
            enrollmentToken = ""
        }
    }

    private var labelSection: some View {
        Section("setup.label.title") {
            Button("setup.scan", systemImage: "qrcode.viewfinder") {
                operationTask = Task {
                    let allowed = await AVCaptureDevice.requestAccess(for: .video)
                    guard !Task.isCancelled else { return }
                    if allowed && DataScannerViewController.isSupported && DataScannerViewController.isAvailable {
                        scanning = true
                    } else { message = String(localized: "setup.error.camera") }
                }
            }
            TextField("setup.label.name", text: $name, prompt: Text(verbatim: "navfeeder-A1B2C3"))
                .accessibilityIdentifier("setup.device_name")
            SecureField("setup.label.password", text: $password)
                .accessibilityIdentifier("setup.password")
            Toggle("setup.label.remember", isOn: $remember)
            Button("setup.label.load") {
                operationTask = Task {
                    do {
                        password = try await credentials.password(for: name) ?? ""
                        message = password.isEmpty ? String(localized: "setup.label.not_saved") : nil
                    } catch { message = error.localizedDescription }
                }
            }
            Button("setup.label.forget", role: .destructive) {
                operationTask = Task {
                    do { try await credentials.forget(name); password = ""; message = nil }
                    catch { message = error.localizedDescription }
                }
            }
            Button("setup.connect") { operationTask = Task { await connect() } }
                .accessibilityIdentifier("setup.connect")
        }
        .disabled(provisioner.busy)
    }

    private var networkSection: some View {
        Section("setup.wifi.title") {
            if !provisioner.networks.isEmpty {
                Picker("setup.wifi.networks", selection: $ssid) {
                    Text("setup.wifi.manual").tag(provisioner.networks.contains(ssid) ? "" : ssid)
                    ForEach(provisioner.networks, id: \.self) { Text(verbatim: $0).tag($0) }
                }
            }
            TextField("setup.wifi.ssid", text: $ssid)
            SecureField("setup.wifi.password", text: $wifiPassword)
        }
        .disabled(provisioner.busy || checking)
    }

    private var collectorSection: some View {
        Section("setup.collector.title") {
            TextField("setup.collector.host", text: $host).keyboardType(.URL)
            TextField("setup.collector.port", text: $port).keyboardType(.numberPad)
            TextField("setup.collector.station", text: $stationID)
            SecureField("setup.collector.token", text: $enrollmentToken)
            Text("setup.collector.note").font(.caption).foregroundStyle(.secondary)
        }
        .disabled(provisioner.busy || checking)
    }

    private func connect() async {
        message = nil
        let label = ObserverSetupLabel(ver: "v1", name: name, username: name, pop: password, transport: "ble")
        await provisioner.connect(label)
        guard !Task.isCancelled, provisioner.stage == .ready else { return }
        if remember {
            do { try await credentials.remember(label) }
            catch { message = error.localizedDescription }
        }
        password = ""
    }

    private func provision() async {
        message = nil
        guard let port = UInt16(port) else { message = ObserverSetupError.invalidConfig.localizedDescription; return }
        let config = ObserverSetupConfig(host: host, port: port, stationID: stationID, enrollmentToken: enrollmentToken)
        do { _ = try config.encoded() } catch { message = error.localizedDescription; return }
        checking = true
        confirmation = nil
        confirmationSession = nil
        // Establish a collector-clock baseline before sending configuration.
        // Setup can still proceed when private read access is unavailable.
        if let session = controller.store.activeSession, session.audience.isPrivate {
            if let envelope = try? await feed.fetchObservers(session: session),
               let payload = envelope.data, payload.schema == "2.0", payload.audience == session.audience.rawValue,
               (try? payload.validate()) != nil, let time = WireDate.parse(envelope.time),
               controller.store.activeSession == session, !Task.isCancelled {
                let board = payload.observers?.first(where: { $0.id == stationID })?.board
                confirmation = ObserverSetupConfirmation(stationID: stationID,
                    previousBoot: (board?.latest ?? board?.timing)?.session, collectorTime: time)
                confirmationSession = session
            }
        }
        checking = false
        guard !Task.isCancelled else { return }
        await provisioner.provision(config, ssid: ssid, password: wifiPassword)
        if provisioner.stage == .awaitingCollector {
            enrollmentToken = ""
            wifiPassword = ""
            if confirmation != nil { await checkCollector() }
        }
    }

    private func checkCollector() async {
        guard let confirmation, let session = confirmationSession,
              session == controller.store.activeSession else {
            message = String(localized: "setup.confirm.private_required"); return
        }
        checking = true
        defer { checking = false }
        do {
            let envelope = try await feed.fetchObservers(session: session)
            guard !Task.isCancelled, controller.store.activeSession == session else { return }
            guard let payload = envelope.data, payload.schema == "2.0", payload.audience == session.audience.rawValue
            else { throw FeedError.invalidResponse }
            try payload.validate()
            guard confirmation.matches(payload) else {
                message = String(localized: "setup.confirm.pending"); return
            }
            try controller.addStation(id: confirmation.stationID, for: session)
            provisioner.confirm()
            message = nil
            await controller.refresh()
        } catch { message = error.localizedDescription }
    }
}

private struct SetupLabelScanner: UIViewControllerRepresentable {
    let completion: (Result<String, ObserverSetupError>) -> Void

    func makeUIViewController(context: Context) -> DataScannerViewController {
        let scanner = DataScannerViewController(recognizedDataTypes: [.barcode(symbologies: [.qr])],
                                                recognizesMultipleItems: false, isHighlightingEnabled: true)
        scanner.delegate = context.coordinator
        do { try scanner.startScanning() }
        catch { Task { @MainActor in completion(.failure(.invalidLabel)) } }
        return scanner
    }

    func updateUIViewController(_ controller: DataScannerViewController, context: Context) {}
    static func dismantleUIViewController(_ controller: DataScannerViewController, coordinator: Coordinator) {
        controller.stopScanning()
        controller.delegate = nil
    }
    func makeCoordinator() -> Coordinator { Coordinator(completion: completion) }

    final class Coordinator: NSObject, DataScannerViewControllerDelegate {
        let completion: (Result<String, ObserverSetupError>) -> Void
        private var finished = false
        init(completion: @escaping (Result<String, ObserverSetupError>) -> Void) { self.completion = completion }
        func dataScanner(_ scanner: DataScannerViewController, didAdd addedItems: [RecognizedItem], allItems: [RecognizedItem]) {
            guard !finished else { return }
            for item in addedItems {
                if case .barcode(let barcode) = item, let value = barcode.payloadStringValue {
                    finished = true
                    scanner.stopScanning()
                    completion(.success(value))
                    return
                }
            }
        }
        func dataScanner(_ scanner: DataScannerViewController, becameUnavailableWithError error: DataScannerViewController.ScanningUnavailable) {
            guard !finished else { return }
            finished = true
            completion(.failure(.invalidLabel))
        }
    }
}
#endif
