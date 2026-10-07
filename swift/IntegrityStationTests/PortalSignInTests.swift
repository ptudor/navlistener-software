import Foundation
import SwiftUI
import Testing
@testable import IntegrityStation

/// The browser sign-in needs the window that owns its button. Without one it
/// says so, instead of starting a session against a detached window and then
/// reporting the portal's "invalid response".
@MainActor @Suite struct PortalSignInTests {
    @Test func signInWithoutAWindowReportsThatItCannotPresent() async throws {
        let browser = PortalBrowserSignIn()
        #expect(browser.anchor == nil)
        await #expect(throws: FeedError.signInUnavailable) {
            _ = try await browser.signIn(at: URL(string: "https://portal.example.invalid/stations/accounts/native/authorize/")!)
        }
        #expect(PortalErrorMessage.describe(FeedError.signInUnavailable) == String(localized: "portal.cannot_present"))
        #expect(!FeedError.signInUnavailable.isAuthorizationLoss)
    }

    #if os(macOS)
    @Test func readerReportsTheWindowThatHostsTheView() async throws {
        let window = NSWindow(contentRect: NSRect(x: -10000, y: -10000, width: 300, height: 200),
                              styleMask: [.titled], backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        let browser = PortalBrowserSignIn()
        let hosting = NSHostingView(rootView: Text(verbatim: "sign in")
            .background(PresentationAnchorReader { browser.anchor = $0 }))
        window.contentView = hosting
        window.orderFront(nil)
        defer { window.contentView = nil; window.close() }
        hosting.layoutSubtreeIfNeeded()
        #expect(try await eventually { browser.anchor === window })
    }
    #endif
}
