package lyricsstaging

import "fmt"

func catalogPerformerPolicyError(musicID int, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("music %d catalog performer segmentation policy: %w", musicID, err)
}
