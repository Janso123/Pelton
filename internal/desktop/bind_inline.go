package desktop

import "github.com/peltonapp/Pelton/internal/mailview"

// settingInlineScanned records that the one-time scan for mail cached while the
// parser dropped Outlook's inline pictures has run. The scan reads every body
// with a cid: reference, so it happens once rather than on every start.
const settingInlineScanned = "inline_parts_rescan_done"

// markMissingInlineMail finds cached messages whose html points at a cid:
// picture they have no attachment for and marks them to be fetched again. The
// raw source is not kept, so the next sync of their folder, or opening one,
// brings the missing parts.
func (a *App) markMissingInlineMail() {
	if a.boolSetting(settingInlineScanned, false) {
		return
	}
	found, err := a.store.MarkMissingInlineParts(a.ctx, mailview.ReferencedCIDs)
	if err != nil {
		a.log.Error("scan cached mail for missing inline pictures", "err", err)
		return
	}
	if err := a.store.SetBool(a.ctx, settingInlineScanned, true); err != nil {
		a.log.Error("record inline picture scan", "err", err)
	}
	if found > 0 {
		a.log.Info("marked cached mail missing inline pictures for refetch", "count", found)
	}
}
