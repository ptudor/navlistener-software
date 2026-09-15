import Foundation
import Testing
@testable import IntegrityStation

@Test func setupLabelRequiresThePhysicalS3Contract() throws {
    let valid = #"{"ver":"v1","name":"navfeeder-A1B2C3","username":"navfeeder-A1B2C3","pop":"FakePass2345","transport":"ble"}"#
    #expect(try ObserverSetupLabel.parse(valid).name == "navfeeder-A1B2C3")
    for invalid in [valid.replacingOccurrences(of: "v1", with: "v2"),
                    valid.replacingOccurrences(of: "ble", with: "softap"),
                    valid.replacingOccurrences(of: "A1B2C3", with: "a1b2c3"),
                    valid.replacingOccurrences(of: "FakePass2345", with: "short"),
                    valid.replacingOccurrences(of: #""username":"navfeeder-A1B2C3""#, with: #""username":"different""#),
                    String(repeating: "x", count: 513)] {
        #expect(throws: (any Error).self) { try ObserverSetupLabel.parse(invalid) }
    }
}

@Test func provisioningRequestMatchesFirmwareNVF1BytesAndLimits() throws {
    let config = ObserverSetupConfig(host: "collector.invalid", port: 5580, stationID: "roof", enrollmentToken: "ingest-token")
    let data = try config.encoded()
    #expect(Array(data.prefix(10)) == [0x4e, 0x56, 0x46, 0x31, 0, 0x15, 0xcc, 17, 4, 12])
    #expect(String(data: data.dropFirst(10), encoding: .utf8) == "collector.invalidroofingest-token")
    let largest = ObserverSetupConfig(host: String(repeating: "h", count: 63), port: 65535,
                                     stationID: String(repeating: "s", count: 32), enrollmentToken: String(repeating: "t", count: 128))
    #expect(try largest.encoded().count == 233)
    for bad in [ObserverSetupConfig(host: "collector.invalid", port: 0, stationID: "s", enrollmentToken: "t"),
                ObserverSetupConfig(host: "https://collector.invalid", port: 1, stationID: "s", enrollmentToken: "t"),
                ObserverSetupConfig(host: "collector.invalid", port: 1, stationID: "s", enrollmentToken: "token\n"),
                ObserverSetupConfig(host: "collector.invalid", port: 1, stationID: String(repeating: "s", count: 33), enrollmentToken: "t"),
                ObserverSetupConfig(host: "collector.invalid", port: 1, stationID: "é", enrollmentToken: "t")] {
        #expect(throws: (any Error).self) { try bad.encoded() }
    }
    // Printable station IDs/tokens are preserved exactly, including whitespace.
    #expect(try ObserverSetupConfig(host: "collector.invalid", port: 1, stationID: " s ", enrollmentToken: " t ").encoded().suffix(6) == Data(" s  t ".utf8))
}

@Test func provisioningResponsesAndSecurityNegotiationFailClosed() throws {
    for status in UInt8(0)...4 {
        #expect(try ObserverSetupReply.parse(Data([0x4e, 0x56, 0x52, 0x31, status])).rawValue == status)
    }
    for invalid in [Data(), Data("NVR1".utf8), Data([0x4e, 0x56, 0x52, 0x31, 5]), Data("NVR10extra".utf8)] {
        #expect(throws: (any Error).self) { try ObserverSetupReply.parse(invalid) }
    }
    let app: [String: Any] = ["cap": ["nav-config-v1"]]
    #expect(ObserverSetupContract.supports(["prov": ["sec_ver": 2], "navfeeder": app]))
    #expect(!ObserverSetupContract.supports(["prov": ["sec_ver": 1], "navfeeder": app]))
    #expect(!ObserverSetupContract.supports(["prov": ["sec_ver": 0], "navfeeder": app]))
    #expect(!ObserverSetupContract.supports(["prov": ["sec_ver": 2], "navfeeder": ["cap": ["other"]]]))
    #expect(!ObserverSetupContract.supports(nil))
}

@MainActor private final class SetupTransportStub: ObserverProvisioningTransport {
    var reply = ObserverSetupReply.waitingForWiFi
    var wifiError: ObserverSetupError?
    var calls: [String] = []
    var gate: ConnectionBarrier?
    func connect(label: ObserverSetupLabel) async throws { calls.append("connect") }
    func networks() async throws -> [String] {
        if let gate { await gate.hold() }
        return ["Test Wi-Fi"]
    }
    func configure(_ data: Data) async throws -> ObserverSetupReply { calls.append("config"); return reply }
    func provisionWiFi(ssid: String, password: String) async throws {
        calls.append("wifi")
        if let wifiError { throw wifiError }
    }
    func disconnect() { calls.append("disconnect") }
}

@MainActor @Suite struct ObserverProvisionerTests {
    private var label: ObserverSetupLabel {
        ObserverSetupLabel(ver: "v1", name: "navfeeder-A1B2C3", username: "navfeeder-A1B2C3", pop: "FakePass2345", transport: "ble")
    }
    private var config: ObserverSetupConfig {
        ObserverSetupConfig(host: "collector.invalid", port: 5580, stationID: "roof", enrollmentToken: "ingest-token")
    }

    @Test func wifiSuccessAndDisconnectBothRequireCollectorConfirmation() async {
        for failure in [nil, ObserverSetupError.connection] {
            let transport = SetupTransportStub()
            transport.wifiError = failure
            let setup = ObserverProvisioner(transport: transport)
            await setup.connect(label)
            await setup.provision(config, ssid: "Test Wi-Fi", password: "test-password")
            #expect(setup.stage == .awaitingCollector)
            #expect(transport.calls.suffix(3) == ["config", "wifi", "disconnect"])
            #expect(setup.stationID == "roof")
            setup.confirm()
            #expect(setup.stage == .confirmed)
        }
    }

    @Test func failedSaveNeverSendsWiFiAndFailedWiFiCanRetry() async {
        let transport = SetupTransportStub()
        let setup = ObserverProvisioner(transport: transport)
        await setup.connect(label)
        transport.reply = .saveFailed
        await setup.provision(config, ssid: "Test Wi-Fi", password: "test-password")
        #expect(setup.stage == .ready)
        #expect(!transport.calls.contains("wifi"))
        transport.reply = .waitingForWiFi
        transport.wifiError = .wifi
        await setup.provision(config, ssid: "Test Wi-Fi", password: "test-password")
        #expect(setup.stage == .ready)
        transport.wifiError = nil
        await setup.provision(config, ssid: "Test Wi-Fi", password: "test-password")
        #expect(setup.stage == .awaitingCollector)
    }

    @Test func cancellingSetupRetiresALateScanResult() async {
        let transport = SetupTransportStub()
        let gate = ConnectionBarrier()
        transport.gate = gate
        let setup = ObserverProvisioner(transport: transport)
        let task = Task { await setup.connect(label) }
        await gate.waitUntilEntered()
        setup.cancel()
        await gate.release()
        await task.value
        #expect(setup.stage == .idle)
        #expect(setup.networks.isEmpty)
    }
}

@Test func confirmationRequiresANewBootReceivedAfterSetupStarted() throws {
    let payload = try BoardFixture.payload()
    let now = try #require(WireDate.parse("2026-09-14T11:59:59Z"))
    #expect(ObserverSetupConfirmation(stationID: "observer-s3", previousBoot: "boot-old", collectorTime: now).matches(payload))
    #expect(!ObserverSetupConfirmation(stationID: "observer-s3", previousBoot: "boot-a", collectorTime: now).matches(payload))
    #expect(!ObserverSetupConfirmation(stationID: "other", previousBoot: nil, collectorTime: now).matches(payload))
    #expect(!ObserverSetupConfirmation(stationID: "observer-s3", previousBoot: nil, collectorTime: now.addingTimeInterval(2)).matches(payload))
}
