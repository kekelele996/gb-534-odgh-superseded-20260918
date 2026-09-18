package dto
import (
	"encoding/json"
	"fermentation-kinetics-deviation-analysis/backend/internal/constants"
	"fermentation-kinetics-deviation-analysis/backend/internal/model"
	"fermentation-kinetics-deviation-analysis/backend/internal/util"
	"time"
)
type RunDeviationAnalysisRequest struct {
	SensorSeriesID uint `json:"sensor_series_id" binding:"required"`
}
type DeviationAnalysisTransitionRequest struct {
	ToState string `json:"to_state" binding:"required,oneof=reviewed confirmed investigating voided"`
	Comment string `json:"comment" binding:"omitempty,max=1000"`
}
type DeviationAnalysisQuery struct {
	SensorSeriesID, RecipeID uint
	State, Level, Initiator  string
	Page, PageSize           int
}

// Actions a viewer is allowed to perform on the analysis in its current state,
// after role and three-role separation (initiator/reviewer/confirmer) checks.
const (
	ActionReview     = "review"
	ActionReturn     = "return_investigation"
	ActionConfirm    = "confirm"
	ActionVoid       = "void"
)
type DeviationAnalysisResponse struct {
	ID                   uint                  `json:"id"`
	SensorSeriesID       uint                  `json:"sensor_series_id"`
	RecipeID             uint                  `json:"recipe_id"`
	RecipeVersion        int                   `json:"recipe_version"`
	AlgorithmVersion     string                `json:"algorithm_version"`
	InputHash            string                `json:"input_hash"`
	PhaseScoresJSON      json.RawMessage       `json:"phase_scores_json"`
	DeviationLevel       string                `json:"deviation_level"`
	AlignedCurveJSON     json.RawMessage       `json:"aligned_curve_json"`
	SuspectedCausesJSON  json.RawMessage       `json:"suspected_causes_json"`
	AnalysisState        string                `json:"analysis_state"`
	Explanation          string                `json:"explanation"`
	AnalyzedAt           time.Time             `json:"analyzed_at"`
	InitiatedBy          uint                  `json:"initiated_by"`
	InitiatedByName      string                `json:"initiated_by_name"`
	ReviewedBy           *uint                 `json:"reviewed_by,omitempty"`
	ReviewedByName       string                `json:"reviewed_by_name,omitempty"`
	ReviewedAt           *time.Time            `json:"reviewed_at,omitempty"`
	ConfirmedBy          *uint                 `json:"confirmed_by,omitempty"`
	ConfirmedByName      string                `json:"confirmed_by_name,omitempty"`
	ConfirmedAt          *time.Time            `json:"confirmed_at,omitempty"`
	ReturnReason         string                `json:"return_reason,omitempty"`
	DurationMilliseconds int64                 `json:"duration_milliseconds"`
	FailureReason        string                `json:"failure_reason,omitempty"`
	ReviewComment        string                `json:"review_comment,omitempty"`
	ReplayVerified       *bool                 `json:"replay_verified,omitempty"`
	AvailableActions     []string              `json:"available_actions"`
	SensorSeries         *SensorSeriesResponse `json:"sensor_series,omitempty"`
	CreatedAt            time.Time             `json:"created_at"`
	UpdatedAt            time.Time             `json:"updated_at"`
}
type DeviationAnalysisListResponse struct {
	Items []DeviationAnalysisResponse `json:"items"`
	Total int64                       `json:"total"`
	Page  int                         `json:"page"`
	Size  int                         `json:"page_size"`
}
func NewDeviationAnalysisResponse(analysis model.DeviationAnalysis) DeviationAnalysisResponse {
	return NewDeviationAnalysisResponseFor(analysis, util.Actor{})
}
func NewDeviationAnalysisResponseFor(analysis model.DeviationAnalysis, actor util.Actor) DeviationAnalysisResponse {
	response := DeviationAnalysisResponse{
		ID: analysis.ID, SensorSeriesID: analysis.SensorSeriesID, RecipeID: analysis.RecipeID,
		RecipeVersion: analysis.RecipeVersion, AlgorithmVersion: analysis.AlgorithmVersion,
		InputHash: analysis.InputHash, PhaseScoresJSON: rawJSON(analysis.PhaseScoresJSON),
		DeviationLevel: analysis.DeviationLevel, AlignedCurveJSON: rawJSON(analysis.AlignedCurveJSON),
		SuspectedCausesJSON: rawJSON(analysis.SuspectedCausesJSON), AnalysisState: analysis.AnalysisState,
		Explanation: analysis.Explanation, AnalyzedAt: analysis.AnalyzedAt,
		InitiatedBy: analysis.InitiatedBy, InitiatedByName: analysis.InitiatedByName,
		ReviewedBy: analysis.ReviewedBy, ReviewedByName: analysis.ReviewedByName, ReviewedAt: analysis.ReviewedAt,
		ConfirmedBy: analysis.ConfirmedBy, ConfirmedByName: analysis.ConfirmedByName, ConfirmedAt: analysis.ConfirmedAt,
		ReturnReason: analysis.ReturnReason,
		DurationMilliseconds: analysis.DurationMilliseconds, FailureReason: analysis.FailureReason,
		ReviewComment: analysis.ReviewComment, ReplayVerified: analysis.ReplayVerified,
		AvailableActions: AvailableActions(analysis, actor),
		CreatedAt: analysis.CreatedAt, UpdatedAt: analysis.UpdatedAt,
	}
	if analysis.SensorSeries.ID != 0 {
		s := NewSensorSeriesResponse(analysis.SensorSeries)
		response.SensorSeries = &s
	}
	return response
}

// AvailableActions computes the actions a specific viewer may trigger right now.
// It mirrors the service-level checks so the UI can hide or disable operations
// that the API would reject; the API remains authoritative.
func AvailableActions(analysis model.DeviationAnalysis, actor util.Actor) []string {
	if actor.UserID == 0 {
		return []string{}
	}
	role := constants.Role(actor.Role)
	canReview := constants.HasPermission(role, constants.PermissionAnalysisReview)
	canConfirm := constants.HasPermission(role, constants.PermissionAnalysisConfirm)
	actions := make([]string, 0, 4)
	switch constants.AnalysisState(analysis.AnalysisState) {
	case constants.AnalysisCompleted:
		if canReview && analysis.InitiatorSeparated(actor.UserID) {
			actions = append(actions, ActionReview)
		}
		if canReview {
			actions = append(actions, ActionVoid)
		}
	case constants.AnalysisReviewed:
		if canReview {
			actions = append(actions, ActionReturn, ActionVoid)
		}
		if canConfirm && analysis.ConfirmationEligible(actor.UserID) {
			actions = append(actions, ActionConfirm)
		}
	case constants.AnalysisInvestigating:
		if canReview && analysis.InitiatorSeparated(actor.UserID) {
			actions = append(actions, ActionReview)
		}
		if canReview {
			actions = append(actions, ActionVoid)
		}
	case constants.AnalysisConfirmed:
		if canReview {
			actions = append(actions, ActionVoid)
		}
	}
	return actions
}
