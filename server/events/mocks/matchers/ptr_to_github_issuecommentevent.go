// Code generated by pegomock. DO NOT EDIT.
package matchers

import (
	"reflect"
	"github.com/petergtz/pegomock"
	github "github.com/google/go-github/v28/github"
)

func AnyPtrToGithubIssueCommentEvent() *github.IssueCommentEvent {
	pegomock.RegisterMatcher(pegomock.NewAnyMatcher(reflect.TypeOf((*(*github.IssueCommentEvent))(nil)).Elem()))
	var nullValue *github.IssueCommentEvent
	return nullValue
}

func EqPtrToGithubIssueCommentEvent(value *github.IssueCommentEvent) *github.IssueCommentEvent {
	pegomock.RegisterMatcher(&pegomock.EqMatcher{Value: value})
	var nullValue *github.IssueCommentEvent
	return nullValue
}