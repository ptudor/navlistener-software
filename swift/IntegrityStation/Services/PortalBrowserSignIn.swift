import AuthenticationServices
import SwiftUI

/// Runs the owner-portal browser sign-in from the window that owns the
/// sign-in button. ASWebAuthenticationSession needs a real presentation
/// anchor: a detached `NSWindow()`/`UIWindow()` made `start()` return false,
/// and that failure then read as an invalid portal response.
@MainActor final class PortalBrowserSignIn: NSObject, ASWebAuthenticationPresentationContextProviding {
    /// The window hosting the view that starts the sign-in, supplied by
    /// `PresentationAnchorReader`. The window owns its views, not the other
    /// way round, so this is weak; without one the sign-in is not started.
    weak var anchor: ASPresentationAnchor?
    private var session: ASWebAuthenticationSession?
    private var continuation: CheckedContinuation<URL, Error>?
    private var identifier = UUID()

    func signIn(at url: URL) async throws -> URL {
        cancel()
        guard anchor != nil else { throw FeedError.signInUnavailable }
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
                if !session.start() { finish(url: nil, error: FeedError.signInUnavailable) }
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
        // A session only starts with the anchor in hand. Should its window go
        // away while the session is up there is nothing left to present from;
        // the session then fails and finish() reports it.
        anchor ?? ASPresentationAnchor()
    }
}

/// Reports the window that hosts a SwiftUI view, so a browser sign-in can be
/// anchored to the window that owns its button rather than to a detached one.
/// Zero-sized; place it in a view's background.
struct PresentationAnchorReader: View {
    let onWindow: @MainActor (ASPresentationAnchor?) -> Void

    var body: some View {
        AnchorRepresentable(onWindow: onWindow)
            .frame(width: 0, height: 0)
            .accessibilityHidden(true)
    }
}

#if os(macOS)
private struct AnchorRepresentable: NSViewRepresentable {
    let onWindow: @MainActor (ASPresentationAnchor?) -> Void
    func makeNSView(context: Context) -> AnchorView { AnchorView(onWindow: onWindow) }
    func updateNSView(_ view: AnchorView, context: Context) { view.onWindow = onWindow }
}

private final class AnchorView: NSView {
    var onWindow: @MainActor (ASPresentationAnchor?) -> Void
    init(onWindow: @escaping @MainActor (ASPresentationAnchor?) -> Void) {
        self.onWindow = onWindow
        super.init(frame: .zero)
    }
    @available(*, unavailable) required init?(coder: NSCoder) { nil }
    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow()
        onWindow(window)
    }
}
#else
private struct AnchorRepresentable: UIViewRepresentable {
    let onWindow: @MainActor (ASPresentationAnchor?) -> Void
    func makeUIView(context: Context) -> AnchorView { AnchorView(onWindow: onWindow) }
    func updateUIView(_ view: AnchorView, context: Context) { view.onWindow = onWindow }
}

private final class AnchorView: UIView {
    var onWindow: @MainActor (ASPresentationAnchor?) -> Void
    init(onWindow: @escaping @MainActor (ASPresentationAnchor?) -> Void) {
        self.onWindow = onWindow
        super.init(frame: .zero)
        isUserInteractionEnabled = false
    }
    @available(*, unavailable) required init?(coder: NSCoder) { nil }
    override func didMoveToWindow() {
        super.didMoveToWindow()
        onWindow(window)
    }
}
#endif
