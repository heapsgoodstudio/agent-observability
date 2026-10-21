package installer

import (
	"fmt"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

var planBox = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	Padding(0, 2)

// RunWizard displays the installation plan. Profiles and history are configured
// automatically or through Settings; neither is an installation question.
func RunWizard(r Report, in *Inputs, yes bool, backfillFlag string) error {
	in.Backfill = backfillFlag != "no" && (!r.Installed || backfillFlag == "yes")

	descs := Describe(BuildPlan(*in))
	fmt.Println(planBox.Render("Install plan\n\n  " + strings.Join(descs, "\n  ")))
	if yes {
		return nil
	}
	proceed := false
	if err := huh.NewConfirm().Title("Proceed?").Affirmative("Install").Negative("Abort").
		Value(&proceed).Run(); err != nil {
		return err
	}
	if !proceed {
		return fmt.Errorf("aborted")
	}
	return nil
}
