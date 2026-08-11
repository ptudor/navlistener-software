import CryptoKit
import Foundation
import Security

protocol SecureConnectionStoring: Sendable {
    func lastServerURL() async throws -> String?
    func saveLastServerURL(_ value: String) async throws
    func token(forServer server: String) async throws -> String?
    func saveToken(_ token: String, forServer server: String) async throws
    func deleteToken(forServer server: String) async throws
}

enum SecureConnectionError: Error, LocalizedError, Sendable {
    case keychain(OSStatus)
    case invalidValue

    var errorDescription: String? {
        switch self {
        case .keychain:
            String(localized: "error.secure_storage")
        case .invalidValue:
            String(localized: "error.secure_storage_value")
        }
    }
}

/// Stores the last collector URL and server-scoped read tokens in the platform
/// Keychain. Observer ingest credentials are never accepted by this client.
actor KeychainConnectionStore: SecureConnectionStoring {
    private let service: String

    init(service: String = "net.intsat.station.read-connection.v1") {
        self.service = service
    }

    func lastServerURL() throws -> String? {
        try read(account: "last-server")
    }

    func saveLastServerURL(_ value: String) throws {
        try write(value, account: "last-server")
    }

    func token(forServer server: String) throws -> String? {
        try read(account: tokenAccount(server))
    }

    func saveToken(_ token: String, forServer server: String) throws {
        guard ReadSession.isValidToken(token) else { throw SecureConnectionError.invalidValue }
        try write(token, account: tokenAccount(server))
    }

    func deleteToken(forServer server: String) throws {
        let status = SecItemDelete(baseQuery(account: tokenAccount(server)) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw SecureConnectionError.keychain(status)
        }
    }

    private func read(account: String) throws -> String? {
        var query = baseQuery(account: account)
        query[kSecReturnData] = true
        query[kSecMatchLimit] = kSecMatchLimitOne
        var result: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess else { throw SecureConnectionError.keychain(status) }
        guard let data = result as? Data, let value = String(data: data, encoding: .utf8) else {
            throw SecureConnectionError.invalidValue
        }
        return value
    }

    private func write(_ value: String, account: String) throws {
        guard let data = value.data(using: .utf8) else { throw SecureConnectionError.invalidValue }
        let query = baseQuery(account: account)
        let update = [kSecValueData: data] as CFDictionary
        let status = SecItemUpdate(query as CFDictionary, update)
        if status == errSecItemNotFound {
            var item = query
            item[kSecValueData] = data
            item[kSecAttrAccessible] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
            let addStatus = SecItemAdd(item as CFDictionary, nil)
            guard addStatus == errSecSuccess else { throw SecureConnectionError.keychain(addStatus) }
        } else if status != errSecSuccess {
            throw SecureConnectionError.keychain(status)
        }
    }

    private func baseQuery(account: String) -> [CFString: Any] {
        [
            kSecClass: kSecClassGenericPassword,
            kSecAttrService: service,
            kSecAttrAccount: account,
        ]
    }

    private func tokenAccount(_ server: String) -> String {
        let digest = SHA256.hash(data: Data(server.utf8))
        return "token:" + digest.map { String(format: "%02x", $0) }.joined()
    }
}
