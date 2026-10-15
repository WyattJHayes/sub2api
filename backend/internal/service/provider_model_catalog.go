package service

// DefaultNativeProviderModelIDs is shared by Codex model listing and the plaza.
// Other providers use their existing compatibility catalogs or account mappings.
func DefaultNativeProviderModelIDs(platform string) []string {
	switch platform {
	case PlatformDeepseek:
		return []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-flash"}
	case PlatformMiniMax:
		return []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5"}
	default:
		return nil
	}
}
