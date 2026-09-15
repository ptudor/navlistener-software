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
    for status in UInt8(0)...5 {
        #expect(try ObserverSetupReply.parse(Data([0x4e, 0x56, 0x52, 0x31, status])).rawValue == status)
    }
    for invalid in [Data(), Data("NVR1".utf8), Data([0x4e, 0x56, 0x52, 0x31, 6]), Data("NVR10extra".utf8)] {
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
    var tunnelReply = ObserverSetupReply.waitingForWiFi
    var wifiError: ObserverSetupError?
    var calls: [String] = []
    var gate: ConnectionBarrier?
    var supportsTunnel = false
    func connect(label: ObserverSetupLabel) async throws { calls.append("connect") }
    func networks() async throws -> [String] {
        if let gate { await gate.hold() }
        return ["Test Wi-Fi"]
    }
    func configure(_ data: Data) async throws -> ObserverSetupReply { calls.append("config"); return reply }
    func configureTunnel(_ data: Data) async throws -> ObserverSetupReply { calls.append("tunnel"); return tunnelReply }
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

private let tunnelProfile = """
[Interface]
PrivateKey = AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=
Address = 10.77.0.12/24

[Peer]
PublicKey = ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=
Endpoint = wg.collector.invalid:51820
AllowedIPs = 10.77.0.1/32
PersistentKeepalive = 25
"""

@Test func tunnelProfileParsesAndEncodesTheFirmwareNVT1Bytes() throws {
    let config = try ObserverTunnelConfig.parse(tunnelProfile)
    #expect(config.endpointHost == "wg.collector.invalid" && config.endpointPort == 51820)
    #expect(config.address == (10, 77, 0, 12) && config.prefix == 24)
    #expect(config.collector == (10, 77, 0, 1) && config.keepalive == 25)
    let data = Array(try config.encoded())
    #expect(data.count == 135)
    #expect(Array(data.prefix(5)) == [0x4e, 0x56, 0x54, 0x31, 0])
    #expect(data[5] == 0 && data[37] == 0x20)        // first byte of each decoded key
    #expect(Array(data[69..<101]) == Array(repeating: 0, count: 32)) // no preshared key
    #expect(Array(data[101...105]) == [10, 77, 0, 12, 24])
    #expect(Array(data[106...109]) == [10, 77, 0, 1])
    #expect(data[110] == 0xca && data[111] == 0x6c)  // port 51820, big-endian
    #expect(data[112] == 0 && data[113] == 25)       // keepalive 25
    #expect(data[114] == 20)
    #expect(String(bytes: data[115...], encoding: .utf8) == "wg.collector.invalid")

    // A preshared key and "off" keepalive both round-trip; a bare Address is a /32 host.
    let withPSK = tunnelProfile
        .replacingOccurrences(of: "PersistentKeepalive = 25", with: "PresharedKey = QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8=\nPersistentKeepalive = off")
        .replacingOccurrences(of: "Address = 10.77.0.12/24", with: "Address = 10.77.0.12")
    let cfg2 = try ObserverTunnelConfig.parse(withPSK)
    #expect(cfg2.keepalive == 0 && cfg2.prefix == 32 && cfg2.presharedKey != Data(count: 32))
    let ip = try ObserverTunnelConfig.parse(tunnelProfile.replacingOccurrences(
        of: "Endpoint = wg.collector.invalid:51820", with: "Endpoint = 203.0.113.5:443"))
    #expect(ip.endpointHost == "203.0.113.5" && ip.endpointPort == 443)
}

@Test func tunnelProfileRejectsMalformedInput() throws {
    let bad = [
        "",                                                                  // empty
        tunnelProfile.replacingOccurrences(of: "AllowedIPs = 10.77.0.1/32", with: "AllowedIPs = 10.77.0.0/24"),   // not a /32
        tunnelProfile.replacingOccurrences(of: "AllowedIPs = 10.77.0.1/32", with: "AllowedIPs = 0.0.0.0/0"),      // default route
        tunnelProfile.replacingOccurrences(of: "AllowedIPs = 10.77.0.1/32", with: "AllowedIPs = 10.77.0.12/32"),  // equals self
        tunnelProfile.replacingOccurrences(of: "Endpoint = wg.collector.invalid:51820", with: "Endpoint = wg.collector.invalid"), // no port
        tunnelProfile.replacingOccurrences(of: "Endpoint = wg.collector.invalid:51820", with: "Endpoint = [2001:db8::1]:51820"),  // IPv6
        tunnelProfile.replacingOccurrences(of: "Address = 10.77.0.12/24", with: "Address = fd00::12/64"),         // IPv6 address
        tunnelProfile.replacingOccurrences(of: "PrivateKey = AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", with: "PrivateKey = not-a-key"),
        tunnelProfile.replacingOccurrences(of: "[Peer]", with: "[Peer]\nColour = blue"),   // unknown key
        tunnelProfile + "\n[Peer]\n",                                        // second section
        String(repeating: "#comment\n", count: 200),                        // over 1 KiB
    ]
    for profile in bad {
        #expect(throws: ObserverSetupError.self) { try ObserverTunnelConfig.parse(profile) }
    }
    // A canonical WireGuard key is 44 base64 chars, '=' padded, standard alphabet.
    #expect(throws: (any Error).self) { try ObserverTunnelConfig.decodeKey("short") }
    #expect(throws: (any Error).self) {
        try ObserverTunnelConfig.decodeKey("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh_=") // url-safe alphabet
    }
}

@MainActor @Test func provisioningSendsTheTunnelBetweenCollectorAndWiFi() async throws {
    let config = ObserverTunnelConfig(privateKey: Data(0..<32), peerPublicKey: Data(32..<64),
        presharedKey: Data(count: 32), endpointHost: "wg.collector.invalid", endpointPort: 51820,
        address: (10, 77, 0, 12), prefix: 24, collector: (10, 77, 0, 1), keepalive: 25)
    let transport = SetupTransportStub()
    transport.supportsTunnel = true
    let setup = ObserverProvisioner(transport: transport)
    let label = ObserverSetupLabel(ver: "v1", name: "navfeeder-A1B2C3", username: "navfeeder-A1B2C3", pop: "FakePass2345", transport: "ble")
    await setup.connect(label)
    #expect(setup.supportsTunnel)
    let collector = ObserverSetupConfig(host: "collector.invalid", port: 5580, stationID: "roof", enrollmentToken: "ingest-token")
    await setup.provision(collector, ssid: "Test Wi-Fi", password: "test-password", tunnel: config)
    #expect(setup.stage == .awaitingCollector)
    #expect(transport.calls.suffix(4) == ["config", "tunnel", "wifi", "disconnect"])
}

@MainActor @Test func tunnelIsRefusedWhenTheDeviceLacksSupportOrArrivesTooLate() async throws {
    let config = ObserverTunnelConfig(privateKey: Data(0..<32), peerPublicKey: Data(32..<64),
        presharedKey: Data(count: 32), endpointHost: "wg.collector.invalid", endpointPort: 51820,
        address: (10, 77, 0, 12), prefix: 24, collector: (10, 77, 0, 1), keepalive: 25)
    let label = ObserverSetupLabel(ver: "v1", name: "navfeeder-A1B2C3", username: "navfeeder-A1B2C3", pop: "FakePass2345", transport: "ble")
    let collector = ObserverSetupConfig(host: "collector.invalid", port: 5580, stationID: "roof", enrollmentToken: "ingest-token")

    // The device did not advertise nav-tunnel: the profile is refused before any BLE call.
    let noSupport = SetupTransportStub()
    let a = ObserverProvisioner(transport: noSupport)
    await a.connect(label)
    #expect(!a.supportsTunnel)
    await a.provision(collector, ssid: "Test Wi-Fi", password: "pw", tunnel: config)
    #expect(a.stage == .ready)
    #expect(!noSupport.calls.contains("tunnel") && !noSupport.calls.contains("wifi"))

    // The firmware already saved before the tunnel arrived: the app surfaces it, no Wi-Fi.
    let late = SetupTransportStub()
    late.supportsTunnel = true
    late.tunnelReply = .alreadySaved
    let b = ObserverProvisioner(transport: late)
    await b.connect(label)
    await b.provision(collector, ssid: "Test Wi-Fi", password: "pw", tunnel: config)
    #expect(b.stage == .ready)
    #expect(late.calls.contains("tunnel") && !late.calls.contains("wifi"))
}

@Test func confirmationRequiresANewBootReceivedAfterSetupStarted() throws {
    let payload = try BoardFixture.payload()
    let now = try #require(WireDate.parse("2026-09-14T11:59:59Z"))
    #expect(ObserverSetupConfirmation(stationID: "observer-s3", previousBoot: "boot-old", collectorTime: now).matches(payload))
    #expect(!ObserverSetupConfirmation(stationID: "observer-s3", previousBoot: "boot-a", collectorTime: now).matches(payload))
    #expect(!ObserverSetupConfirmation(stationID: "other", previousBoot: nil, collectorTime: now).matches(payload))
    #expect(!ObserverSetupConfirmation(stationID: "observer-s3", previousBoot: nil, collectorTime: now.addingTimeInterval(2)).matches(payload))
}
