import SwiftUI

struct InstrumentBackground: View {
    @Environment(\.colorScheme) private var colorScheme

    var body: some View {
        (colorScheme == .dark ? StationPalette.darkBackground : StationPalette.lightBackground)
            .overlay(alignment: .topTrailing) {
                RadialGradient(
                    colors: [Color.accentColor.opacity(0.10), .clear],
                    center: .topTrailing,
                    startRadius: 0,
                    endRadius: 520
                )
            }
            .ignoresSafeArea()
    }
}

struct InstrumentCard<Content: View>: View {
    @Environment(\.colorScheme) private var colorScheme
    let title: LocalizedStringKey?
    let systemImage: String?
    @ViewBuilder let content: Content

    init(
        _ title: LocalizedStringKey? = nil,
        systemImage: String? = nil,
        @ViewBuilder content: () -> Content
    ) {
        self.title = title
        self.systemImage = systemImage
        self.content = content()
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if let title {
                Label {
                    Text(title).font(.headline)
                } icon: {
                    if let systemImage { Image(systemName: systemImage) }
                }
                .foregroundStyle(.primary)
            }
            content
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            colorScheme == .dark ? StationPalette.darkSurface : StationPalette.lightSurface,
            in: RoundedRectangle(cornerRadius: 18, style: .continuous)
        )
        .overlay {
            RoundedRectangle(cornerRadius: 18, style: .continuous)
                .strokeBorder(.primary.opacity(colorScheme == .dark ? 0.10 : 0.08))
        }
        .shadow(color: .black.opacity(colorScheme == .dark ? 0.18 : 0.06), radius: 12, y: 6)
    }
}

struct HealthLabel: View {
    let state: HealthState

    var body: some View {
        Label(state.localizedName, systemImage: state.systemImage)
            .foregroundStyle(StationPalette.health(state))
            .font(.subheadline.weight(.semibold))
    }
}

struct FeedErrorBanner: View {
    let message: String

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: "exclamationmark.icloud.fill")
                .foregroundStyle(StationPalette.warning)
            Text(message)
                .font(.callout)
                .foregroundStyle(.primary)
            Spacer(minLength: 0)
        }
        .padding(12)
        .background(StationPalette.warning.opacity(0.12), in: RoundedRectangle(cornerRadius: 12))
        .accessibilityElement(children: .combine)
    }
}

struct MetricRow: View {
    let label: LocalizedStringKey
    let value: String
    var monospaced = false

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            Text(label).foregroundStyle(.secondary)
            Spacer(minLength: 12)
            Text(value)
                .font(monospaced ? .callout.monospaced() : .callout.monospacedDigit())
                .multilineTextAlignment(.trailing)
                .textSelection(.enabled)
        }
        .font(.callout)
    }
}

struct ConstellationBadge: View {
    @Environment(\.colorScheme) private var colorScheme
    let gnssid: Int
    let count: Int

    var body: some View {
        HStack(spacing: 5) {
            Circle()
                .fill(StationPalette.constellation(gnssid: gnssid, scheme: colorScheme))
                .frame(width: 7, height: 7)
            Text(ConstellationName.localized(gnssid: gnssid))
            Text("\(count)")
                .font(.caption.monospacedDigit().weight(.semibold))
        }
        .font(.caption)
        .padding(.horizontal, 8)
        .padding(.vertical, 5)
        .background(.primary.opacity(0.06), in: Capsule())
        .accessibilityElement(children: .combine)
    }
}
