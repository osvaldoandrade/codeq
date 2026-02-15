import { Module } from '@nestjs/common';
import { ConfigModule } from '@nestjs/config';
import { ScheduleModule } from '@nestjs/schedule';
import { CodeQModule } from './config/codeq.module';
import { TasksModule } from './tasks/tasks.module';
import { WorkersModule } from './workers/workers.module';

/**
 * Root application module.
 * 
 * Imports:
 * - ConfigModule: Environment configuration
 * - ScheduleModule: Cron jobs for workers
 * - CodeQModule: CodeQ client configuration
 * - TasksModule: Task creation endpoints
 * - WorkersModule: Background task processing
 */
@Module({
  imports: [
    ConfigModule.forRoot({
      isGlobal: true,
    }),
    ScheduleModule.forRoot(),
    CodeQModule,
    TasksModule,
    WorkersModule,
  ],
})
export class AppModule {}
