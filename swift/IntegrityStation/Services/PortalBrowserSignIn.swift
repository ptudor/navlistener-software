import AuthenticationServices
import SwiftUI

@MainActor final class PortalBrowserSignIn: NSObject, ASWebAuthenticationPresentationContextProviding {
    private var session: ASWebAuthenticationSession?
    private var continuation: CheckedContinuation<URL, Error>?
    private var identifier = UUID()
    func signIn(at url: URL) async throws -> URL {
        cancel()
        let identifier = UUID(); self.identifier = identifier
        return try await withTaskCancellationHandler {
            try Task.checkCancellation()
            return try await withCheckedThrowingContinuation { continuation in
                self.continuation = continuation
                let session = ASWebAuthenticationSession(url: url, callbackURLScheme: PortalAuthorization.clientID) { [weak self] url, error in
                    Task { @MainActor in
                        guard self?.identifier == identifier else { return }
                        self?.finish(url: url, error: error)
                    }
                }
                session.presentationContextProvider = self
                self.session = session
                if !session.start() { finish(url: nil, error: FeedError.invalidResponse) }
            }
        } onCancel: { Task { @MainActor in self.cancel() } }
    }
    func cancel() { identifier = UUID(); session?.cancel(); finish(url: nil, error: CancellationError()) }
    private func finish(url: URL?, error: Error?) {
        let pending = continuation; continuation = nil; session = nil
        if let url { pending?.resume(returning: url) }
        else if let failure = error as? ASWebAuthenticationSessionError, failure.code == .canceledLogin {
            pending?.resume(throwing: CancellationError())
        }
        else { pending?.resume(throwing: error ?? CancellationError()) }
    }
    func presentationAnchor(for session: ASWebAuthenticationSession) -> ASPresentationAnchor {
        #if os(macOS)
        NSApp.keyWindow ?? NSApp.windows.first ?? NSWindow()
        #else
        UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }.flatMap(\.windows).first(where: \.isKeyWindow) ?? UIWindow()
        #endif
    }
}
