import { Module } from '@nestjs/common';
import { TasksController } from './tasks.controller';
import { TasksService } from './tasks.service';

/**
 * Module for task creation (producer role).
 */
@Module({
  controllers: [TasksController],
  providers: [TasksService],
})
export class TasksModule {}
