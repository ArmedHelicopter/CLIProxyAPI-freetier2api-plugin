package main

// 本文件是定时任务族的通用入口：遍历账号、检查签到能力、把其余任务族
// 交给供应商的 core.ScheduledRunner 执行。
//
// 「签到 + 从属闭环（连登兑换、开学季）」的时序属于通用编排：
// 签到提升连登天数 → 解锁兑换档位 → 兑换产出抽奖次数。

import (
	"context"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

// runCheckinAll 执行全账号签到，并在启用自动任务时驱动从属闭环。
func runCheckinAll(ctx context.Context) {
	if !effectiveAutoCheckin() {
		logger.Debug("checkin skipped: auto checkin is disabled")
		return
	}
	logger.Info("checkin: starting for all accounts")
	count := 0
	forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		if !core.SupportsCheckin(account.vendor) {
			return nil
		}
		checkin, okCheckin := core.As[core.CheckinProvider](account.vendor)
		if !okCheckin {
			return nil
		}
		result, errCheckin := checkin.Checkin(ctx, account.credential)
		if errCheckin != nil {
			logger.Error("checkin %s: %v", account.label(), errCheckin)
			return nil
		}
		if result == nil {
			return nil
		}
		recordCheckinResult(account.credential, result)
		if result.Already {
			logger.Info("checkin %s: already checked in today", account.label())
		} else {
			logger.Info("checkin %s: %s", account.label(), result.Message)
		}
		count++
		return nil
	})
	logger.Info("checkin: finished for %d account(s)", count)

	if effectiveAutoTasks() {
		runScheduledFamily(ctx, core.TaskKindStreak)
		runScheduledFamily(ctx, core.TaskKindSchool)
	}
}

// runScheduledFamily 遍历账号，把任务族交给供应商的可选实现。
func runScheduledFamily(ctx context.Context, kind string) {
	logger.Info("%s: starting for all accounts", kind)
	count := 0
	forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		runner, okRunner := core.As[core.ScheduledRunner](account.vendor)
		if !okRunner {
			return nil
		}
		message, errRun := runner.RunScheduled(ctx, account.credential, kind)
		if errRun != nil {
			logger.Error("%s %s: %v", kind, account.label(), errRun)
			return nil
		}
		if message != "" {
			logger.Info("%s %s: %s", kind, account.label(), message)
			count++
		}
		return nil
	})
	logger.Info("%s: finished for %d account(s)", kind, count)
}

// runActivityAll 执行全账号活跃上报。
func runActivityAll(ctx context.Context) { runScheduledFamily(ctx, core.TaskKindActivity) }

// runKeepaliveAll 执行全账号 token 保活。
func runKeepaliveAll(ctx context.Context) { runScheduledFamily(ctx, core.TaskKindKeepalive) }

// runTravelAll 执行全账号猫猫旅行。
func runTravelAll(ctx context.Context) { runScheduledFamily(ctx, core.TaskKindTravel) }

// runBlackcatAll 执行夜猫子任务补足。
func runBlackcatAll(ctx context.Context) { runScheduledFamily(ctx, core.TaskKindBlackcat) }

// runSchoolAll 执行开学季活动闭环。
func runSchoolAll(ctx context.Context) { runScheduledFamily(ctx, core.TaskKindSchool) }

// runStreakBonusAll 执行连登兑换与抽奖闭环。
func runStreakBonusAll(ctx context.Context) { runScheduledFamily(ctx, core.TaskKindStreak) }
